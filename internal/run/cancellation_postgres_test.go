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
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCancelerTerminalizesAuthorityAndLeavesDetachedChildren(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	parent := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	detached := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, ctx, fixture.pool, `
UPDATE runs
   SET cause_kind = 'child',
       parent_run_id = $1,
       parent_owns_lifecycle = false
 WHERE id = $2`, parent.runID, detached.runID)

	canceler, err := NewCanceler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	result, err := canceler.Cancel(ctx, CancellationRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID,
		EnvironmentID: fixture.environmentID,
		RunID:         parent.runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.CancelledRuns != 1 || result.RunID != parent.runID {
		t.Fatalf("cancellation result = %+v", result)
	}

	assertCancellationState(t, fixture, parent, "cancelled", "cancelled", "fenced")
	var detachedStatus db.RunStatus
	if err := fixture.pool.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1`, detached.runID,
	).Scan(&detachedStatus); err != nil {
		t.Fatal(err)
	}
	if detachedStatus != db.RunStatusQueued {
		t.Fatalf("detached child status = %s, want queued", detachedStatus)
	}
	var ownerRunID *uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
SELECT owner_run_id
  FROM workspaces
 WHERE id = (SELECT workspace_id FROM runs WHERE id = $1)`, parent.runID,
	).Scan(&ownerRunID); err != nil {
		t.Fatal(err)
	}
	if ownerRunID != nil {
		t.Fatalf("cancelled root retained Workspace ownership: %s", *ownerRunID)
	}
	var desiredState, mountState string
	if err := fixture.pool.QueryRow(ctx, `
SELECT runtime_instances.desired_state, workspace_mounts.state
  FROM run_leases
  JOIN runtime_instances ON runtime_instances.id = run_leases.runtime_instance_id
  JOIN workspace_mounts ON workspace_mounts.runtime_instance_id = runtime_instances.id
 WHERE run_leases.id = $1`, parent.leaseID,
	).Scan(&desiredState, &mountState); err != nil {
		t.Fatal(err)
	}
	if desiredState != "closed" || mountState != "unmounting" {
		t.Fatalf("cleanup state = runtime:%s mount:%s", desiredState, mountState)
	}

	replay, err := canceler.Cancel(ctx, CancellationRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID,
		EnvironmentID: fixture.environmentID,
		RunID:         result.RunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Changed || replay.CancelledRuns != 0 || replay.RunID != parent.runID {
		t.Fatalf("cancellation replay = %+v", replay)
	}
}

func TestCancelerRejectsTargetOutsideScope(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	work := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	canceler, err := NewCanceler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	requests := []CancellationRequest{
		{
			OrgID: uuid.New(), ProjectID: fixture.projectID,
			EnvironmentID: fixture.environmentID, RunID: work.runID,
		},
		{
			OrgID: fixture.orgID, ProjectID: uuid.New(),
			EnvironmentID: fixture.environmentID, RunID: work.runID,
		},
		{
			OrgID: fixture.orgID, ProjectID: fixture.projectID,
			EnvironmentID: uuid.New(), RunID: work.runID,
		},
	}
	for _, request := range requests {
		if _, err := canceler.Cancel(ctx, request); !errors.Is(err, ErrCancellationNotFound) {
			t.Fatalf("out-of-scope cancellation error = %v", err)
		}
	}
	var status db.RunStatus
	if err := fixture.pool.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1`, work.runID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusQueued {
		t.Fatalf("out-of-scope cancellation changed Run status to %s", status)
	}
}

func TestOwnedFinalizationFailsSecretRevokedRun(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	work := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	graph, err := LockOwnedFinalization(
		ctx,
		tx,
		OwnedFinalizationRequest{
			OrgID:         fixture.orgID,
			ProjectID:     fixture.projectID,
			EnvironmentID: fixture.environmentID,
			RunID:         work.runID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	terminalized, err := graph.FailCurrentForSecretRevocation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if terminalized != 1 {
		t.Fatalf("terminalized Runs = %d, want 1", terminalized)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var runStatus db.RunStatus
	var runReason string
	var runError []byte
	var attemptOutcome, attemptReason string
	var runLeaseState, workspaceLeaseState string
	if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status,
	   runs.failure->>'code',
	   runs.failure,
       run_attempts.terminal_outcome,
       run_attempts.terminal_reason_code,
       run_leases.state,
       workspace_leases.state
  FROM runs
  JOIN run_attempts
    ON run_attempts.run_id = runs.id
   AND run_attempts.number = runs.current_attempt_number
  JOIN run_leases
    ON run_leases.run_id = runs.id
   AND run_leases.id = $2
  JOIN workspace_leases
    ON workspace_leases.owner_run_lease_id = run_leases.id
 WHERE runs.id = $1`,
		work.runID,
		work.leaseID,
	).Scan(
		&runStatus,
		&runReason,
		&runError,
		&attemptOutcome,
		&attemptReason,
		&runLeaseState,
		&workspaceLeaseState,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusFailed || runReason != "secret_revoked" ||
		attemptOutcome != "failed" || attemptReason != "secret_revoked" ||
		runLeaseState != "rejected" || workspaceLeaseState != "fenced" {
		t.Fatalf(
			"Secret-revoked authority = run:%s/%s attempt:%s/%s leases:%s/%s",
			runStatus,
			runReason,
			attemptOutcome,
			attemptReason,
			runLeaseState,
			workspaceLeaseState,
		)
	}
	var failurePayload map[string]any
	if err := json.Unmarshal(runError, &failurePayload); err != nil {
		t.Fatal(err)
	}
	if failurePayload["code"] != "secret_revoked" {
		t.Fatalf("Run failure = %#v", failurePayload)
	}
}

func TestOwnedFinalizationBoundsRuntimePreparationForEveryRun(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	work := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	var runtimeID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
SELECT runtime_instance_id FROM run_leases WHERE id = $1`, work.leaseID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, fixture.pool,
		`DELETE FROM workspace_leases WHERE owner_run_lease_id = $1`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool, `
UPDATE runs
   SET current_run_lease_id = NULL, first_lease_at = NULL
 WHERE id = $1`, work.runID)
	dbtest.MustExec(t, ctx, fixture.pool,
		`DELETE FROM run_leases WHERE id = $1`, work.leaseID)
	dbtest.MustExec(t, ctx, fixture.pool,
		`DELETE FROM workspace_mounts WHERE runtime_instance_id = $1`, runtimeID)
	dbtest.MustExec(t, ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'preparing', observed_version = 2,
       observed_desired_version = 0, ready_at = NULL,
       reserved_run_id = $2, reserved_attempt_number = 1,
       reserved_workspace_version_id = (
           SELECT base_workspace_version_id FROM runs WHERE id = $2
       ), reservation_expires_at = now() + interval '5 minutes'
 WHERE id = $1`, runtimeID, work.runID)

	for failure := int32(1); failure <= 8; failure++ {
		chargedAfter := time.Now()
		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		graph, err := LockOwnedFinalization(ctx, tx, OwnedFinalizationRequest{
			OrgID: fixture.orgID, ProjectID: fixture.projectID,
			EnvironmentID: fixture.environmentID, RunID: work.runID,
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		exhausted, err := graph.ChargeRuntimePreparationFailure(ctx)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if exhausted != (failure == 8) {
			t.Fatalf("failure %d exhausted = %t", failure, exhausted)
		}
		var count int32
		var next pgtype.Timestamptz
		var status db.RunStatus
		if err := fixture.pool.QueryRow(ctx, `
SELECT runtime_preparation_count, next_runtime_preparation_at, status
  FROM runs WHERE id = $1`, work.runID).Scan(&count, &next, &status); err != nil {
			t.Fatal(err)
		}
		if count != failure {
			t.Fatalf("failure %d count = %d", failure, count)
		}
		if failure < 8 {
			if !next.Valid || status != db.RunStatusQueued {
				t.Fatalf("failure %d authority = next:%v status:%s", failure, next, status)
			}
			delaySeconds := []int{2, 4, 8, 16, 32, 60, 60}[failure-1]
			minimum := chargedAfter.Add(time.Duration(delaySeconds-1) * time.Second)
			maximum := time.Now().Add(time.Duration(delaySeconds+1) * time.Second)
			if next.Time.Before(minimum) || next.Time.After(maximum) {
				t.Fatalf("failure %d next = %s, want about %ds", failure, next.Time, delaySeconds)
			}
		} else if next.Valid || status != db.RunStatusSystemFailed {
			t.Fatalf("exhausted authority = next:%v status:%s", next, status)
		}
	}

	var attemptOutcome, attemptReason string
	var attemptNumber int32
	var ownerRunID pgtype.UUID
	if err := fixture.pool.QueryRow(ctx, `
SELECT run_attempts.terminal_outcome,
       run_attempts.terminal_reason_code,
       runs.current_attempt_number,
       workspaces.owner_run_id
  FROM runs
  JOIN run_attempts ON run_attempts.run_id = runs.id
                   AND run_attempts.number = runs.current_attempt_number
  JOIN workspaces ON workspaces.id = runs.workspace_id
	 WHERE runs.id = $1`, work.runID).Scan(&attemptOutcome, &attemptReason, &attemptNumber, &ownerRunID); err != nil {
		t.Fatal(err)
	}
	if attemptOutcome != "failed" || attemptReason != "runtime_preparation_failed" ||
		attemptNumber != 1 || ownerRunID.Valid {
		t.Fatalf("terminal preparation authority = attempt:%d/%s/%s owner:%v", attemptNumber, attemptOutcome, attemptReason, ownerRunID)
	}
}

func TestOwnedFinalizationExhaustsActorRuntimePreparation(t *testing.T) {
	ctx := t.Context()
	fixture := newPostgresFixture(t)
	work := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	sessionID := fixture.convertToActor(t, ctx, work, `{"enabled":false}`)
	dbtest.MustExec(t, ctx, fixture.pool, `
UPDATE runs
   SET current_run_lease_id = NULL
 WHERE id = $1`, work.runID)

	exhaustRuntimePreparation(t, ctx, fixture, work.runID)

	var runStatus db.RunStatus
	var sessionState string
	var failure []byte
	if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status, sessions.state, sessions.failure
  FROM runs
  JOIN sessions ON sessions.id = $2
 WHERE runs.id = $1`, work.runID, sessionID).Scan(&runStatus, &sessionState, &failure); err != nil {
		t.Fatal(err)
	}
	var payload Failure
	if err := json.Unmarshal(failure, &payload); err != nil {
		t.Fatal(err)
	}
	if runStatus != db.RunStatusSystemFailed || sessionState != "failed" ||
		payload.Code != "platform_failure" {
		t.Fatalf("actor preparation exhaustion = run:%s session:%s failure:%+v", runStatus, sessionState, payload)
	}
}

func TestOwnedFinalizationExhaustsDifferentWorkspaceChildRuntimePreparation(t *testing.T) {
	ctx := t.Context()
	fixture := newPostgresFixture(t)
	parent := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	child := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	claimID := uuid.NewV7()
	waitID := uuid.NewV7()
	dbtest.MustExec(t, ctx, fixture.pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash,
    request_fingerprint, accepted_at
) VALUES ($1, $2, 'task.child.invoke', $3, $4, now())`,
		claimID, fixture.environmentID,
		dbtest.Hash("preparation-child-slot"), dbtest.Hash("preparation-child-request"))
	dbtest.MustExec(t, ctx, fixture.pool, `
UPDATE runs
   SET cause_kind = 'child', parent_run_id = $1,
       parent_owns_lifecycle = true, claim_id = $2,
       current_run_lease_id = NULL
 WHERE id = $3`, parent.runID, claimID, child.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `UPDATE runs SET status = 'waiting' WHERE id = $1`, parent.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind,
    child_run_id, child_parent_owned, child_target_declared_id,
    child_claim_id, child_request, suspension_state,
    expected_run_state_version, attempt_number, current_run_lease_id,
    resume_attach_id
)
SELECT $1, runs.environment_id, runs.id, runs.workspace_id, 'child',
       $2, true, 'test-task', $3, '{}'::jsonb, 'hot',
       runs.state_version, runs.current_attempt_number, runs.current_run_lease_id,
       $4
  FROM runs
 WHERE runs.id = $5`,
		waitID, child.runID, claimID, uuid.NewV7(), parent.runID)

	exhaustRuntimePreparation(t, ctx, fixture, child.runID)

	var childStatus, parentStatus db.RunStatus
	var condition db.WaitState
	var result []byte
	if err := fixture.pool.QueryRow(ctx, `
SELECT child.status, parent.status, wait.condition_state, wait.condition_result
  FROM runs AS child
  JOIN runs AS parent ON parent.id = child.parent_run_id
  JOIN run_waits AS wait ON wait.id = $2
 WHERE child.id = $1`, child.runID, waitID).Scan(
		&childStatus, &parentStatus, &condition, &result,
	); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		OK      bool    `json:"ok"`
		Failure Failure `json:"failure"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatal(err)
	}
	if childStatus != db.RunStatusSystemFailed || parentStatus != db.RunStatusRunning ||
		condition != db.WaitStateCompleted || payload.OK ||
		payload.Failure.Code != "runtime_preparation_failed" {
		t.Fatalf(
			"different-workspace preparation exhaustion = child:%s parent:%s wait:%s result:%+v",
			childStatus, parentStatus, condition, payload,
		)
	}
}

func exhaustRuntimePreparation(
	t *testing.T,
	ctx context.Context,
	fixture postgresFixture,
	runID uuid.UUID,
) {
	t.Helper()
	for failure := 1; failure <= 8; failure++ {
		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		graph, err := LockOwnedFinalization(ctx, tx, OwnedFinalizationRequest{
			OrgID: fixture.orgID, ProjectID: fixture.projectID,
			EnvironmentID: fixture.environmentID, RunID: runID,
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		exhausted, err := graph.ChargeRuntimePreparationFailure(ctx)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if exhausted != (failure == 8) {
			t.Fatalf("failure %d exhausted = %t", failure, exhausted)
		}
	}
}

func TestCancelerRejectsAnotherTerminalOutcome(t *testing.T) {
	ctx := context.Background()
	fixture := newPostgresFixture(t)
	work := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, ctx, fixture.pool, `
UPDATE run_attempts
   SET terminal_outcome = 'succeeded',
       terminal_reason_code = 'task_succeeded',
       terminal_at = now()
 WHERE run_id = $1
   AND number = 1`, work.runID)
	dbtest.MustExec(t, ctx, fixture.pool, `
UPDATE runs
   SET status = 'succeeded',
       output = '{}'::jsonb,
       terminal_at = now()
 WHERE id = $1`, work.runID)
	canceler, err := NewCanceler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}

	_, err = canceler.Cancel(ctx, CancellationRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID,
		EnvironmentID: fixture.environmentID,
		RunID:         work.runID,
	})
	if !errors.Is(err, ErrCancellationConflict) {
		t.Fatalf("cancel succeeded Run error = %v", err)
	}
	var status db.RunStatus
	if err := fixture.pool.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1`, work.runID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusSucceeded {
		t.Fatalf("succeeded Run changed to %s", status)
	}
}

func TestCancelerResolvesDifferentWorkspaceChildWait(t *testing.T) {
	for _, suspension := range []db.RunWaitState{
		db.RunWaitStateHot,
		db.RunWaitStateCheckpointing,
	} {
		t.Run(string(suspension), func(t *testing.T) {
			ctx := t.Context()
			fixture := newPostgresFixture(t)
			parent := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
			child := fixture.addRun(t, "assigned", time.Now().Add(-time.Minute))
			claimID := uuid.NewV7()
			waitID := uuid.NewV7()
			dbtest.MustExec(t, ctx, fixture.pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash,
    request_fingerprint, accepted_at
) VALUES (
    $1, $2, 'task.child.invoke', $3, $4, now()
)`,
				claimID,
				fixture.environmentID,
				dbtest.Hash("cancel-child-slot"),
				dbtest.Hash("cancel-child-request"),
			)
			dbtest.MustExec(t, ctx, fixture.pool, `
UPDATE runs
   SET cause_kind = 'child',
       parent_run_id = $1,
       parent_owns_lifecycle = true,
       claim_id = $2
 WHERE id = $3`,
				parent.runID,
				claimID,
				child.runID,
			)
			dbtest.MustExec(t, ctx, fixture.pool, `
UPDATE runs
   SET status = 'waiting'
 WHERE id = $1`,
				parent.runID,
			)
			dbtest.MustExec(t, ctx, fixture.pool, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind,
    child_run_id, child_parent_owned, child_target_declared_id,
    child_claim_id, child_request, suspension_state,
    expected_run_state_version, attempt_number, current_run_lease_id,
    resume_attach_id
)
SELECT $1, runs.environment_id, runs.id, runs.workspace_id, 'child',
       $2, true, 'test-task', $3, '{}'::jsonb, $4,
       runs.state_version, runs.current_attempt_number, runs.current_run_lease_id,
       $5
  FROM runs
 WHERE runs.id = $6`,
				waitID,
				child.runID,
				claimID,
				suspension,
				uuid.NewV7(),
				parent.runID,
			)

			canceler, err := NewCanceler(fixture.pool)
			if err != nil {
				t.Fatal(err)
			}
			result, err := canceler.Cancel(ctx, CancellationRequest{
				OrgID:         fixture.orgID,
				ProjectID:     fixture.projectID,
				EnvironmentID: fixture.environmentID,
				RunID:         child.runID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Changed || result.CancelledRuns != 1 {
				t.Fatalf("cancellation result = %+v", result)
			}

			var parentStatus db.RunStatus
			var condition db.WaitState
			var resolvedSuspension db.RunWaitState
			var conditionResult []byte
			if err := fixture.pool.QueryRow(ctx, `
SELECT runs.status,
       run_waits.condition_state,
       run_waits.suspension_state,
       run_waits.condition_result
  FROM runs
  JOIN run_waits ON run_waits.run_id = runs.id
 WHERE runs.id = $1
   AND run_waits.id = $2`,
				parent.runID,
				waitID,
			).Scan(
				&parentStatus,
				&condition,
				&resolvedSuspension,
				&conditionResult,
			); err != nil {
				t.Fatal(err)
			}
			expectedStatus := db.RunStatusWaiting
			expectedSuspension := db.RunWaitStateCheckpointing
			if suspension == db.RunWaitStateHot {
				expectedStatus = db.RunStatusRunning
				expectedSuspension = db.RunWaitStateReleased
			}
			if parentStatus != expectedStatus ||
				condition != db.WaitStateCompleted ||
				resolvedSuspension != expectedSuspension {
				t.Fatalf(
					"parent Wait = run:%s condition:%s suspension:%s",
					parentStatus,
					condition,
					resolvedSuspension,
				)
			}
			var payload struct {
				OK      bool    `json:"ok"`
				Failure Failure `json:"failure"`
				Run     struct {
					ID string `json:"id"`
				} `json:"run"`
			}
			if err := json.Unmarshal(conditionResult, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.OK ||
				payload.Failure.Code != "child_run_cancelled" ||
				payload.Failure.Message != "Child Run was cancelled" ||
				len(payload.Failure.Details) != 0 ||
				payload.Run.ID != result.RunID.String() {
				t.Fatalf("child cancellation result = %+v", payload)
			}
		})
	}
}

func assertCancellationState(
	t *testing.T,
	fixture postgresFixture,
	work leasedRun,
	wantRun string,
	wantLease string,
	wantWorkspaceLease string,
) {
	t.Helper()
	var runStatus, leaseState, workspaceLeaseState string
	var attemptOutcome string
	if err := fixture.pool.QueryRow(t.Context(), `
SELECT runs.status,
       run_attempts.terminal_outcome,
       run_leases.state,
       workspace_leases.state
  FROM runs
  JOIN run_attempts
    ON run_attempts.run_id = runs.id
   AND run_attempts.number = runs.current_attempt_number
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
 WHERE runs.id = $1`, work.runID, work.leaseID,
	).Scan(&runStatus, &attemptOutcome, &leaseState, &workspaceLeaseState); err != nil {
		t.Fatal(err)
	}
	if runStatus != wantRun || attemptOutcome != "cancelled" ||
		leaseState != wantLease || workspaceLeaseState != wantWorkspaceLease {
		t.Fatalf(
			"authority state = run:%s attempt:%s lease:%s workspace-lease:%s",
			runStatus, attemptOutcome, leaseState, workspaceLeaseState,
		)
	}
}
