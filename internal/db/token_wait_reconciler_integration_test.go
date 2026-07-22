package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
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
	reconciler, err := NewTokenWaitReconciler(fixture.pool)
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
	if registered.ConditionState != WaitStateCompleted || registered.SuspensionState != RunWaitStateReleased ||
		registered.RunStateVersion != expectedRunVersion+2 || string(registered.Result) != `{"approved": true}` {
		t.Fatalf("registration = %+v", registered)
	}
	replayed, err := reconciler.RegisterWait(ctx, registration)
	if err != nil || replayed.WaitID != registered.WaitID || replayed.ConditionState != registered.ConditionState ||
		replayed.SuspensionState != registered.SuspensionState || string(replayed.Result) != string(registered.Result) {
		t.Fatalf("registration replay = %+v, %v; first = %+v", replayed, err, registered)
	}
	var runStatus RunStatus
	var runVersion int64
	var condition WaitState
	var suspension RunWaitState
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, runs.state_version, run_waits.condition_state, run_waits.suspension_state
		  FROM runs JOIN run_waits ON run_waits.run_id = runs.id
		 WHERE runs.id = $1 AND run_waits.id = $2
	`, work.runID, waitID).Scan(&runStatus, &runVersion, &condition, &suspension); err != nil {
		t.Fatal(err)
	}
	if runStatus != RunStatusRunning || runVersion != expectedRunVersion+2 || condition != WaitStateCompleted || suspension != RunWaitStateReleased {
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
	reconciler, err := NewTokenWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	waitID := uuid.Must(uuid.NewV7())
	registration := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, waitID)
	registered, err := reconciler.RegisterWait(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ConditionState != WaitStatePending || registered.SuspensionState != RunWaitStateHot ||
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
	var status RunStatus
	var condition WaitState
	var suspension RunWaitState
	if err := fixture.pool.QueryRow(ctx, `
		SELECT runs.status, run_waits.condition_state, run_waits.suspension_state
		  FROM runs JOIN run_waits ON run_waits.run_id = runs.id
		 WHERE runs.id = $1 AND run_waits.id = $2
	`, work.runID, waitID).Scan(&status, &condition, &suspension); err != nil {
		t.Fatal(err)
	}
	if status != RunStatusRunning || condition != WaitStateCompleted || suspension != RunWaitStateReleased {
		t.Fatalf("registration-first state = run %s condition %s suspension %s", status, condition, suspension)
	}
}

func TestTokenWaitRegistrationConcurrentReplayConverges(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))
	startTaskCompletionWork(t, ctx, fixture, work)
	tokenID := createTokenTerminalTestToken(t, ctx, fixture, time.Now().Add(time.Hour))
	request := tokenWaitRegistrationRequest(t, ctx, fixture, work, tokenID, uuid.Must(uuid.NewV7()))
	reconciler, err := NewTokenWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	type registrationOutcome struct {
		result TokenWaitRegistrationResult
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
			outcome.result.ConditionState != WaitStatePending ||
			outcome.result.SuspensionState != RunWaitStateHot {
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
	reconciler, err := NewTokenWaitReconciler(fixture.pool)
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
		    state, restore_manifest, ready_at
		) VALUES (
		    $1, 'suspend', $2, 1, $3, $4, $5, $6, $7, $7,
		    'ready', '{"test":true}'::jsonb, transaction_timestamp()
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
	if err != nil || replayed.WaitID != request.WaitID || replayed.ConditionState != WaitStateCancelled ||
		replayed.SuspensionState != RunWaitStateResumePending ||
		replayed.RunStateVersion != registered.RunStateVersion+1 {
		t.Fatalf("parked registration replay = %+v, %v; first = %+v", replayed, err, registered)
	}
	changed := request
	changed.ExpectedRunStateVersion++
	if _, err := reconciler.RegisterWait(ctx, changed); !errors.Is(err, ErrTokenWaitReconcileAuthority) {
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
	reconciler, err := NewTokenWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := reconciler.RegisterWait(ctx, request)
	if err != nil || registered.WaitID != request.WaitID || registered.ConditionState != WaitStatePending {
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
	reconciler, err := NewTokenWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.RegisterWait(ctx, request); !errors.Is(err, ErrTokenWaitReconcileAuthority) {
		t.Fatalf("expired registration error = %v", err)
	}
	var status RunStatus
	var waitCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, (SELECT count(*) FROM run_waits WHERE id = $2)
		  FROM runs WHERE id = $1
	`, work.runID, waitID).Scan(&status, &waitCount); err != nil {
		t.Fatal(err)
	}
	if status != RunStatusRunning || waitCount != 0 {
		t.Fatalf("expired authority mutated run=%s waits=%d", status, waitCount)
	}
}

func TestTokenWaitRegistrationRejectsChildRunUntilChildPlacementIsImplemented(t *testing.T) {
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
	reconciler, err := NewTokenWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.RegisterWait(ctx, request); !errors.Is(err, ErrTokenWaitReconcileAuthority) {
		t.Fatalf("child registration error = %v", err)
	}
	var count int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM run_waits WHERE id = $1`, waitID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("child registration created %d waits", count)
	}
}

func tokenWaitRegistrationRequest(
	t *testing.T,
	ctx context.Context,
	fixture runLeaseClaimFixture,
	work runLeaseWork,
	tokenID uuid.UUID,
	waitID uuid.UUID,
) TokenWaitRegistration {
	t.Helper()
	request := TokenWaitRegistration{
		EnvironmentID: fixture.environmentID, RunID: work.runID, TokenID: tokenID,
		WaitID: waitID, ResumeAttachID: uuid.Must(uuid.NewV7()), AttemptNumber: 1,
		CurrentRunLeaseID: work.leaseID,
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
		setup := newTokenWaitReconcileSetup(t, ctx, fixture, RunWaitStateHot, time.Now().Add(time.Hour))
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
			runStatus: RunStatusRunning, runVersion: 3,
			conditionState: WaitStateCompleted, suspensionState: RunWaitStateReleased,
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
		setup := newTokenWaitReconcileSetup(t, ctx, fixture, RunWaitStateCheckpointing, time.Now().Add(-time.Minute))
		expired, err := fixture.queries.ExpireDueTokens(ctx, pgvalue.UUID(fixture.orgID))
		if err != nil || len(expired) != 1 {
			t.Fatalf("expire Token = rows %d error %v", len(expired), err)
		}

		batch := reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 1 || batch.Deferred != 1 {
			t.Fatalf("batch = %+v", batch)
		}
		assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
			runStatus: RunStatusWaiting, runVersion: 2,
			conditionState: WaitStateFailed, suspensionState: RunWaitStateCheckpointing,
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
		setup := newTokenWaitReconcileSetup(t, ctx, fixture, RunWaitStateParked, time.Now().Add(time.Hour))
		if _, err := fixture.queries.CancelToken(ctx, tokenCancellationParams(fixture, setup.tokenID)); err != nil {
			t.Fatal(err)
		}

		batch := reconcileTokenWaitBatch(t, ctx, fixture, setup.tokenID)
		if batch.Examined != 1 || batch.Resolved != 1 {
			t.Fatalf("batch = %+v", batch)
		}
		assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
			runStatus: RunStatusQueued, runVersion: 3,
			conditionState: WaitStateCancelled, suspensionState: RunWaitStateResumePending,
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

func TestTokenWaitReconcilerRollsBackParkedTransitionWhenResumeIntentFails(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	setup := newTokenWaitReconcileSetup(t, ctx, fixture, RunWaitStateParked, time.Now().Add(time.Hour))
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

	reconciler, err := NewTokenWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.ReconcileBatch(ctx, fixture.environmentID, setup.tokenID, 100); err == nil {
		t.Fatal("parked reconciliation succeeded despite injected outbox failure")
	}
	assertTokenWaitReconcileState(t, ctx, fixture, setup, tokenWaitReconcileWant{
		runStatus: RunStatusWaiting, runVersion: 2,
		conditionState: WaitStatePending, suspensionState: RunWaitStateParked,
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
	suspension RunWaitState,
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
	case RunWaitStateHot:
	case RunWaitStateCheckpointing:
		mustRunLeaseExec(t, ctx, fixture.pool, `
			UPDATE run_waits
			   SET suspension_state = 'checkpointing',
			       checkpoint_request_version = 1
			 WHERE id = $1
		`, setup.waitID)
	case RunWaitStateParked:
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
			    state, restore_manifest, ready_at
			) VALUES (
			    $1, 'suspend', $2, 1, $3, $4, $5, $6, $7, $7,
			    'ready', '{"test":true}'::jsonb, transaction_timestamp()
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
) TokenWaitReconcileBatch {
	t.Helper()
	reconciler, err := NewTokenWaitReconciler(fixture.pool)
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
	runStatus       RunStatus
	runVersion      int64
	conditionState  WaitState
	suspensionState RunWaitState
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
	var runStatus RunStatus
	var runVersion int64
	var runLeaseID pgtype.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, state_version, current_run_lease_id
		  FROM runs
		 WHERE id = $1
	`, setup.runID).Scan(&runStatus, &runVersion, &runLeaseID); err != nil {
		t.Fatal(err)
	}
	var conditionState WaitState
	var suspensionState RunWaitState
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
	setup := newTokenWaitReconcileSetup(t, ctx, fixture, RunWaitStateHot, time.Now().Add(time.Hour))
	reconciler, err := NewTokenWaitReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reconciler.ReconcileBatch(ctx, fixture.environmentID, setup.tokenID, 100)
	if !errors.Is(err, ErrTokenWaitReconcileAuthority) {
		t.Fatalf("pending Token authority error = %v", err)
	}
	var condition WaitState
	if scanErr := fixture.pool.QueryRow(ctx, `SELECT condition_state FROM run_waits WHERE id = $1`, setup.waitID).Scan(&condition); scanErr != nil {
		t.Fatal(scanErr)
	}
	if condition != WaitStatePending {
		t.Fatalf("pending Token changed Wait to %s", condition)
	}
}
