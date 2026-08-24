package dispatch

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestFreshRunLeaseRecoveryRequeuesPrestartAndUnblocksWorkerStartup(t *testing.T) {
	for _, leaseState := range []string{"assigned", "starting"} {
		t.Run(leaseState, func(t *testing.T) {
			fixture, leaseID, runtimeID := prepareFreshRunLease(t)
			if leaseState == "starting" {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'starting', claimed_at = assigned_at
 WHERE id = $1`, leaseID)
			}
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET start_deadline_at = transaction_timestamp() + interval '5 minutes',
       expires_at = transaction_timestamp() + interval '10 minutes'
 WHERE id = $1`, leaseID)
			newServiceID := uuid.Must(uuid.NewV7())
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET state = 'registering', current_epoch = 2, current_service_id = $2,
       epoch_started_at = transaction_timestamp(), activated_at = NULL,
       runtime_identity_id = NULL, substrate_format = '', substrate_contract = '',
       epoch_cpu_millis = 0, epoch_memory_bytes = 0,
       epoch_guest_ephemeral_disk_bytes = 0, per_vm_cpu_millis = 0,
       per_vm_memory_bytes = 0, per_vm_guest_ephemeral_disk_bytes = 0,
       max_vm_slots = 0, max_runtime_starts = 0,
       cpu_environment = NULL, cpu_environment_digest = NULL,
       observed_at = NULL, run_paused_reason = NULL, runtime_paused_reason = NULL
 WHERE id = $1`, fixture.workerID, newServiceID)

			if _, err := db.New(fixture.pool).CompleteWorkerStartupRecovery(
				fixture.ctx,
				db.CompleteWorkerStartupRecoveryParams{
					WorkerInstanceID: pgvalue.UUID(fixture.workerID),
					WorkerGroupID:    fixture.groupID,
					WorkerEpoch:      pgtype.Int8{Int64: 2, Valid: true},
					RecoveryEvidence: []byte(`{"observed_at":"2026-08-16T00:00:00Z","quarantined":[]}`),
				},
			); err == nil {
				t.Fatal("startup recovery completed before the stale Run lease was recovered")
			}

			recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if recovered != 1 {
				t.Fatalf("recovered = %d, want 1", recovered)
			}

			var runStatus, runLeaseState, workspaceLeaseState, runtimeDesiredState string
			var currentRunLeaseID pgtype.UUID
			var attemptTerminalAt pgtype.Timestamptz
			var attemptNumber int32
			if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.current_attempt_number, runs.current_run_lease_id,
       run_attempts.terminal_at, run_leases.state, workspace_leases.state,
       runtime_instances.desired_state
  FROM runs
  JOIN run_attempts ON run_attempts.run_id = runs.id
                   AND run_attempts.number = runs.current_attempt_number
  JOIN run_leases ON run_leases.id = $2
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
  JOIN runtime_instances ON runtime_instances.id = $3
 WHERE runs.id = $1`, fixture.runID, leaseID, runtimeID).Scan(
				&runStatus, &attemptNumber, &currentRunLeaseID, &attemptTerminalAt,
				&runLeaseState, &workspaceLeaseState, &runtimeDesiredState,
			); err != nil {
				t.Fatal(err)
			}
			if runStatus != "queued" || attemptNumber != 1 || currentRunLeaseID.Valid ||
				attemptTerminalAt.Valid || runLeaseState != "lost" ||
				workspaceLeaseState != "fenced" || runtimeDesiredState != "closed" {
				t.Fatalf("recovery state run=%s attempt=%d current=%v attempt_terminal=%v lease=%s workspace_lease=%s runtime=%s",
					runStatus, attemptNumber, currentRunLeaseID, attemptTerminalAt,
					runLeaseState, workspaceLeaseState, runtimeDesiredState)
			}
			if _, err := db.New(fixture.pool).CompleteWorkerStartupRecovery(
				fixture.ctx,
				db.CompleteWorkerStartupRecoveryParams{
					WorkerInstanceID: pgvalue.UUID(fixture.workerID),
					WorkerGroupID:    fixture.groupID,
					WorkerEpoch:      pgtype.Int8{Int64: 2, Valid: true},
					RecoveryEvidence: []byte(`{"observed_at":"2026-08-16T00:00:00Z","quarantined":[]}`),
				},
			); err != nil {
				t.Fatalf("startup recovery remained blocked: %v", err)
			}
			if recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10); err != nil || recovered != 0 {
				t.Fatalf("recovery replay = %d, %v; want 0, nil", recovered, err)
			}
		})
	}
}

func TestFreshRunLeaseRecoveryChargesExactRuntimeFailureBudget(t *testing.T) {
	for _, test := range []struct {
		name         string
		initialCount int
		wantStatus   string
		wantCount    int
		wantTerminal bool
	}{
		{name: "backoff", initialCount: 0, wantStatus: "queued", wantCount: 1},
		{name: "exhaustion", initialCount: 7, wantStatus: "system_failed", wantCount: 8, wantTerminal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, leaseID, runtimeID := prepareFreshRunLease(t)
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'starting', claimed_at = assigned_at,
       start_deadline_at = transaction_timestamp() + interval '5 minutes',
       expires_at = transaction_timestamp() + interval '10 minutes'
 WHERE id = $1`, leaseID)
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs SET runtime_preparation_count = $2 WHERE id = $1`,
				fixture.runID, test.initialCount)
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runtime_instances
   SET observed_state = 'failed', failed_at = transaction_timestamp(),
       terminal_at = transaction_timestamp(), terminal_reason_code = 'workspace_mount_failed'
 WHERE id = $1`, runtimeID)

			recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if recovered != 1 {
				t.Fatalf("recovered = %d, want 1", recovered)
			}
			var status, leaseState string
			var preparationCount int
			var currentLease pgtype.UUID
			var nextPreparation, runTerminal, attemptTerminal pgtype.Timestamptz
			var attemptReason pgtype.Text
			if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.runtime_preparation_count,
       runs.current_run_lease_id, runs.next_runtime_preparation_at,
       runs.terminal_at, run_attempts.terminal_at,
       run_attempts.terminal_reason_code, run_leases.state
  FROM runs
  JOIN run_attempts ON run_attempts.run_id = runs.id
                   AND run_attempts.number = runs.current_attempt_number
  JOIN run_leases ON run_leases.id = $2
 WHERE runs.id = $1`, fixture.runID, leaseID).Scan(
				&status, &preparationCount, &currentLease, &nextPreparation,
				&runTerminal, &attemptTerminal, &attemptReason, &leaseState,
			); err != nil {
				t.Fatal(err)
			}
			if status != test.wantStatus || preparationCount != test.wantCount ||
				currentLease.Valid || leaseState != "lost" ||
				runTerminal.Valid != test.wantTerminal ||
				attemptTerminal.Valid != test.wantTerminal {
				t.Fatalf("recovery status=%s count=%d current=%v next=%v run_terminal=%v attempt_terminal=%v reason=%v lease=%s",
					status, preparationCount, currentLease, nextPreparation,
					runTerminal, attemptTerminal, attemptReason, leaseState)
			}
			if test.wantTerminal {
				if nextPreparation.Valid || !attemptReason.Valid ||
					attemptReason.String != "runtime_preparation_failed" {
					t.Fatalf("exhaustion next=%v reason=%v", nextPreparation, attemptReason)
				}
			} else if !nextPreparation.Valid || attemptReason.Valid {
				t.Fatalf("backoff next=%v reason=%v", nextPreparation, attemptReason)
			}
		})
	}
}

func TestFreshRunningLeaseLossAppliesPinnedRetryPolicy(t *testing.T) {
	for _, entrypointEntered := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_entrypoint_ack", true: "after_entrypoint_ack"}[entrypointEntered], func(t *testing.T) {
			fixture, leaseID, _ := prepareFreshRunLease(t)
			retryPolicy := `{"backoff":{"factor":1,"jitter":"none","maxMs":1,"minMs":1},"enabled":true,"maxAttempts":2}`
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running', claimed_at = assigned_at, started_at = assigned_at,
       expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, leaseID)
			dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running', retry_policy = $2::jsonb,
       active_started_at = transaction_timestamp() - interval '10 seconds',
       started_at = COALESCE(started_at, transaction_timestamp() - interval '10 seconds'),
       state_version = state_version + 1
 WHERE id = $1`, fixture.runID, retryPolicy)
			if entrypointEntered {
				dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_attempts SET entrypoint_entered_at = transaction_timestamp()
 WHERE run_id = $1 AND number = 1`, fixture.runID)
			}

			recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if recovered != 1 {
				t.Fatalf("recovered = %d, want 1", recovered)
			}
			var status, leaseState, attemptOneOutcome string
			var currentAttempt int32
			var currentLease pgtype.UUID
			var retryAt, activeStarted pgtype.Timestamptz
			var attempts int
			if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.current_attempt_number, runs.current_run_lease_id,
       runs.retry_at, runs.active_started_at, run_leases.state,
       run_attempts.terminal_outcome,
       (SELECT count(*) FROM run_attempts WHERE run_id = runs.id)
  FROM runs
  JOIN run_leases ON run_leases.id = $2
  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
 WHERE runs.id = $1`, fixture.runID, leaseID).Scan(
				&status, &currentAttempt, &currentLease, &retryAt, &activeStarted,
				&leaseState, &attemptOneOutcome, &attempts,
			); err != nil {
				t.Fatal(err)
			}
			if status != "retry_delayed" || currentAttempt != 2 || currentLease.Valid ||
				!retryAt.Valid || activeStarted.Valid || leaseState != "expired" ||
				attemptOneOutcome != "failed" || attempts != 2 {
				t.Fatalf("retry state status=%s attempt=%d current=%v retry_at=%v active=%v lease=%s outcome=%s attempts=%d",
					status, currentAttempt, currentLease, retryAt, activeStarted,
					leaseState, attemptOneOutcome, attempts)
			}
		})
	}
}

func TestCheckpointingLeaseLossInvalidatesSuspensionAndRetries(t *testing.T) {
	fixture, leaseID, _ := prepareFreshRunLease(t)
	waitID, checkpointID, resumeAttachID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var workspaceLeaseID, baseVersionID pgtype.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT workspace_leases.id, workspace_leases.base_version_id
  FROM workspace_leases
 WHERE workspace_leases.owner_run_lease_id = $1`, leaseID).Scan(&workspaceLeaseID, &baseVersionID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'checkpointing', claimed_at = assigned_at, started_at = assigned_at,
       expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, leaseID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'waiting',
       retry_policy = '{"backoff":{"factor":1,"jitter":"none","maxMs":1,"minMs":1},"enabled":true,"maxAttempts":2}'::jsonb,
       active_started_at = transaction_timestamp() - interval '10 seconds',
       started_at = COALESCE(started_at, transaction_timestamp() - interval '10 seconds'),
       state_version = state_version + 1
 WHERE id = $1`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, due_at,
    expected_run_state_version, attempt_number, current_run_lease_id,
    resume_attach_id, suspension_state
) VALUES ($1, $2, $3, $4, 'timer', transaction_timestamp() + interval '1 hour',
          2, 1, $5, $6, 'checkpointing')`,
		waitID, fixture.environmentID, fixture.runID, fixture.workspaceID, leaseID, resumeAttachID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
INSERT INTO run_checkpoints (
    id, run_id, attempt_number, run_wait_id, source_run_lease_id,
    source_workspace_lease_id, workspace_id, base_workspace_version_id, state
) VALUES ($1, $2, 1, $3, $4, $5, $6, $7, 'creating')`,
		checkpointID, fixture.runID, waitID, leaseID, workspaceLeaseID,
		fixture.workspaceID, baseVersionID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_waits SET suspend_checkpoint_id = $2 WHERE id = $1`, waitID, checkpointID)

	recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	var runStatus, leaseState, waitCondition, waitSuspension, checkpointState string
	var currentAttempt int32
	var currentLease pgtype.UUID
	var activeStarted pgtype.Timestamptz
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.current_attempt_number, runs.current_run_lease_id,
       run_leases.state, run_waits.condition_state, run_waits.suspension_state,
       run_checkpoints.state, runs.active_started_at
  FROM runs
  JOIN run_leases ON run_leases.id = $2
  JOIN run_waits ON run_waits.id = $3
  JOIN run_checkpoints ON run_checkpoints.id = $4
 WHERE runs.id = $1`, fixture.runID, leaseID, waitID, checkpointID).Scan(
		&runStatus, &currentAttempt, &currentLease, &leaseState, &waitCondition,
		&waitSuspension, &checkpointState, &activeStarted,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != "retry_delayed" || currentAttempt != 2 || currentLease.Valid ||
		leaseState != "expired" || waitCondition != "failed" || waitSuspension != "failed" ||
		checkpointState != "invalid" || activeStarted.Valid {
		t.Fatalf("checkpoint recovery run=%s attempt=%d current=%v lease=%s wait=%s/%s checkpoint=%s active=%v",
			runStatus, currentAttempt, currentLease, leaseState, waitCondition,
			waitSuspension, checkpointState, activeStarted)
	}
	if replay, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10); err != nil || replay != 0 {
		t.Fatalf("checkpoint recovery replay = %d, %v; want 0, nil", replay, err)
	}
}

func TestFinalizingLeaseLossPreservesReceiptAndRetries(t *testing.T) {
	fixture, leaseID, _ := prepareFreshRunLease(t)
	operationID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'finalizing', claimed_at = assigned_at, started_at = assigned_at,
       expires_at = transaction_timestamp() - interval '1 second',
       finalization_operation_id = $2, finalization_kind = 'capture',
       finalization_started_at = transaction_timestamp() - interval '2 seconds',
       finalization_request_fingerprint = 'sha256:' || repeat('a', 64)
 WHERE id = $1`, leaseID, operationID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running',
       retry_policy = '{"backoff":{"factor":1,"jitter":"none","maxMs":1,"minMs":1},"enabled":true,"maxAttempts":2}'::jsonb,
       active_started_at = NULL,
       started_at = COALESCE(started_at, transaction_timestamp() - interval '10 seconds'),
       state_version = state_version + 1
 WHERE id = $1`, fixture.runID)

	recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	var runStatus, leaseState, finalizationKind, fingerprint string
	var currentAttempt int32
	var currentLease, retainedOperationID pgtype.UUID
	var finalizationStarted pgtype.Timestamptz
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.current_attempt_number, runs.current_run_lease_id,
       run_leases.state, run_leases.finalization_operation_id,
       run_leases.finalization_kind, run_leases.finalization_started_at,
       run_leases.finalization_request_fingerprint
  FROM runs JOIN run_leases ON run_leases.id = $2
 WHERE runs.id = $1`, fixture.runID, leaseID).Scan(
		&runStatus, &currentAttempt, &currentLease, &leaseState, &retainedOperationID,
		&finalizationKind, &finalizationStarted, &fingerprint,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != "retry_delayed" || currentAttempt != 2 || currentLease.Valid ||
		leaseState != "expired" || retainedOperationID != pgvalue.UUID(operationID) ||
		finalizationKind != "capture" || !finalizationStarted.Valid ||
		fingerprint != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("finalizing recovery run=%s attempt=%d current=%v lease=%s operation=%v kind=%s started=%v fingerprint=%s",
			runStatus, currentAttempt, currentLease, leaseState, retainedOperationID,
			finalizationKind, finalizationStarted, fingerprint)
	}
	if replay, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10); err != nil || replay != 0 {
		t.Fatalf("finalizing recovery replay = %d, %v; want 0, nil", replay, err)
	}
}

func TestFreshRunningLeaseLossWithoutRetryTerminalizesRun(t *testing.T) {
	fixture, leaseID, _ := prepareFreshRunLease(t)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running', claimed_at = assigned_at, started_at = assigned_at,
       expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, leaseID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running', active_started_at = transaction_timestamp() - interval '10 seconds',
       started_at = COALESCE(started_at, transaction_timestamp() - interval '10 seconds'),
       state_version = state_version + 1
 WHERE id = $1`, fixture.runID)

	recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	var status, leaseState, attemptOutcome string
	var currentLease pgtype.UUID
	var terminalAt pgtype.Timestamptz
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.current_run_lease_id, runs.terminal_at,
       run_leases.state, run_attempts.terminal_outcome
  FROM runs
  JOIN run_leases ON run_leases.id = $2
  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
 WHERE runs.id = $1`, fixture.runID, leaseID).Scan(
		&status, &currentLease, &terminalAt, &leaseState, &attemptOutcome,
	); err != nil {
		t.Fatal(err)
	}
	if status != "system_failed" || currentLease.Valid || !terminalAt.Valid ||
		leaseState != "expired" || attemptOutcome != "failed" {
		t.Fatalf("terminal state status=%s current=%v terminal=%v lease=%s attempt=%s",
			status, currentLease, terminalAt, leaseState, attemptOutcome)
	}
}

func TestFreshRunningLeaseHardDeadlineExpiresWithoutRetry(t *testing.T) {
	fixture, leaseID, _ := prepareFreshRunLease(t)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running', claimed_at = assigned_at, started_at = assigned_at,
       start_deadline_at = transaction_timestamp() + interval '5 minutes',
       expires_at = transaction_timestamp() + interval '10 minutes'
 WHERE id = $1`, leaseID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running', max_active_duration_ms = 5000,
       retry_policy = '{"backoff":{"factor":1,"jitter":"none","maxMs":1,"minMs":1},"enabled":true,"maxAttempts":2}'::jsonb,
       active_started_at = transaction_timestamp() - interval '10 seconds',
       started_at = COALESCE(started_at, transaction_timestamp() - interval '10 seconds'),
       state_version = state_version + 1
 WHERE id = $1`, fixture.runID)

	recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	var status, reason string
	var currentAttempt int32
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.current_attempt_number, run_attempts.terminal_reason_code
  FROM runs
  JOIN run_attempts ON run_attempts.run_id = runs.id
                   AND run_attempts.number = runs.current_attempt_number
 WHERE runs.id = $1`, fixture.runID).Scan(&status, &currentAttempt, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || currentAttempt != 1 || reason != "max_active_duration_exceeded" {
		t.Fatalf("deadline recovery status=%s attempt=%d reason=%s", status, currentAttempt, reason)
	}
}

func TestFreshActorRunningLeaseLossAppliesPinnedRetryPolicy(t *testing.T) {
	fixture, leaseID, _ := prepareFreshRunLease(t)
	actorID := convertFreshRunToActor(t, fixture)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running', claimed_at = assigned_at, started_at = assigned_at,
       expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, leaseID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running',
       retry_policy = '{"backoff":{"factor":1,"jitter":"none","maxMs":1,"minMs":1},"enabled":true,"maxAttempts":2}'::jsonb,
       active_started_at = transaction_timestamp() - interval '10 seconds',
       started_at = COALESCE(started_at, transaction_timestamp() - interval '10 seconds'),
       state_version = state_version + 1
 WHERE id = $1`, fixture.runID)

	recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	var runStatus, sessionState string
	var currentAttempt int32
	var currentRunID pgtype.UUID
	var attempts int
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, runs.current_attempt_number, sessions.state,
       sessions.current_run_id,
       (SELECT count(*) FROM run_attempts WHERE run_id = runs.id)
  FROM runs JOIN sessions ON sessions.id = $2
 WHERE runs.id = $1`, fixture.runID, actorID).Scan(
		&runStatus, &currentAttempt, &sessionState, &currentRunID, &attempts,
	); err != nil {
		t.Fatal(err)
	}
	if runStatus != "retry_delayed" || currentAttempt != 2 ||
		sessionState != "open" || currentRunID != pgvalue.UUID(fixture.runID) || attempts != 2 {
		t.Fatalf("Actor retry run=%s attempt=%d session=%s current=%v attempts=%d",
			runStatus, currentAttempt, sessionState, currentRunID, attempts)
	}
}

func TestFreshRunningLeaseRetryFailsClosedWhenSecretAuthorityChanged(t *testing.T) {
	fixture, leaseID, _ := prepareFreshRunLease(t)
	secretID, versionID, resolutionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(fixture.ctx)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO secrets (id, environment_id, name, current_version_id, revocation_generation)
VALUES ($1, $2, 'RECOVERY_TOKEN', $3, 1)`, secretID, fixture.environmentID, versionID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO secret_versions (id, secret_id, version, nonce, ciphertext)
VALUES ($1, $2, 1, decode(repeat('01', 12), 'hex'), decode(repeat('02', 16), 'hex'))`,
		versionID, secretID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO workspace_secrets (
    workspace_id, environment_id, placement_kind, placement_target, secret_id
) VALUES ($1, $2, 'env', 'RECOVERY_TOKEN', $3)`,
		fixture.workspaceID, fixture.environmentID, secretID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO secret_resolutions (
    id, workspace_id, run_id, attempt_number, placement_kind, placement_target,
    secret_id, secret_version_id, revocation_generation
) VALUES ($1, $2, $3, 1, 'env', 'RECOVERY_TOKEN', $4, $5, 1)`,
		resolutionID, fixture.workspaceID, fixture.runID, secretID, versionID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE secrets
   SET state = 'revoked', revoked_at = transaction_timestamp(),
	       current_version_id = NULL, state_version = state_version + 1,
	       revocation_generation = revocation_generation + 1
 WHERE id = $1`, secretID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET state = 'running', claimed_at = assigned_at, started_at = assigned_at,
       expires_at = transaction_timestamp() - interval '1 second'
 WHERE id = $1`, leaseID)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET status = 'running',
       retry_policy = '{"backoff":{"factor":1,"jitter":"none","maxMs":1,"minMs":1},"enabled":true,"maxAttempts":2}'::jsonb,
       active_started_at = transaction_timestamp() - interval '10 seconds',
       started_at = COALESCE(started_at, transaction_timestamp() - interval '10 seconds'),
       state_version = state_version + 1
 WHERE id = $1`, fixture.runID)

	recovered, err := fixture.authority.RecoverRunExecutionLeases(fixture.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	var status, reason string
	var attempts int
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.status, run_attempts.terminal_reason_code,
       (SELECT count(*) FROM run_attempts WHERE run_id = runs.id)
  FROM runs
  JOIN run_attempts ON run_attempts.run_id = runs.id AND run_attempts.number = 1
 WHERE runs.id = $1`, fixture.runID).Scan(&status, &reason, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "system_failed" || reason != "secret_retry_unavailable" || attempts != 1 {
		t.Fatalf("Secret failure status=%s reason=%s attempts=%d", status, reason, attempts)
	}
}

func TestFreshRunLeaseRecoveryCandidateBecomesHealthyBeforeLock(t *testing.T) {
	fixture, leaseID, _ := prepareFreshRunLease(t)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases SET expires_at = transaction_timestamp() - interval '1 second' WHERE id = $1`, leaseID)
	candidates, err := db.New(fixture.pool).ListRunExecutionLeaseRecoveryCandidates(fixture.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET start_deadline_at = transaction_timestamp() + interval '5 minutes',
       expires_at = transaction_timestamp() + interval '10 minutes'
 WHERE id = $1`, leaseID)
	recovered, err := fixture.authority.recoverRunExecutionLease(fixture.ctx, candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	if recovered {
		t.Fatal("healthy Run lease was recovered from a stale candidate")
	}
	var currentLease pgtype.UUID
	var leaseState string
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT runs.current_run_lease_id, run_leases.state
  FROM runs JOIN run_leases ON run_leases.id = $2
 WHERE runs.id = $1`, fixture.runID, leaseID).Scan(&currentLease, &leaseState); err != nil {
		t.Fatal(err)
	}
	if currentLease != leaseID || leaseState != "assigned" {
		t.Fatalf("healthy lease changed: current=%v state=%s", currentLease, leaseState)
	}
}

func prepareFreshRunLease(t *testing.T) (runPlacementFixture, pgtype.UUID, pgtype.UUID) {
	t.Helper()
	fixture := newRunPlacementFixture(t)
	reserved, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementRuntimeReady(t, fixture, reserved.RuntimeInstanceID)
	mounting, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	markRunPlacementMountReady(t, fixture, mounting.WorkspaceMountID)
	granted, err := fixture.authority.PlaceReadyRun(fixture.ctx, fixture.candidate())
	if err != nil {
		t.Fatal(err)
	}
	if !granted.LeaseCreated || !granted.Lease.ID.Valid {
		t.Fatalf("fresh Run lease was not granted: %+v", granted)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE run_leases
   SET assigned_at = transaction_timestamp() - interval '10 minutes',
       start_deadline_at = transaction_timestamp() - interval '9 minutes',
       expires_at = transaction_timestamp() + interval '5 minutes'
 WHERE id = $1`, granted.Lease.ID)
	return fixture, granted.Lease.ID, reserved.RuntimeInstanceID
}

func convertFreshRunToActor(t *testing.T, fixture runPlacementFixture) uuid.UUID {
	t.Helper()
	actorID, actorDefinitionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
ALTER TABLE run_attempts
ALTER CONSTRAINT run_attempts_run_id_entrypoint_kind_workspace_id_fkey
DEFERRABLE INITIALLY DEFERRED`)
	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(fixture.ctx)
	dbtest.MustExec(t, fixture.ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id,
    manifest_version, manifest, manifest_digest
) VALUES ($1, $2, $3, 'actor', 'test-actor', 0, '{}'::jsonb, decode(repeat('06', 32), 'hex'))`,
		actorDefinitionID, fixture.environmentID, fixture.deploymentID)
	dbtest.MustExec(t, fixture.ctx, tx, `
INSERT INTO sessions (
    id, environment_id, actor_declared_id, deployment_definition_id,
    workspace_id, current_run_id, next_input_sequence,
    committed_input_sequence, run_queue_name, run_max_active_duration_ms
) VALUES ($1, $2, 'test-actor', $3, $4, $5, 2, 1, 'default', 300000)`,
		actorID, fixture.environmentID, actorDefinitionID, fixture.workspaceID, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE runs
   SET deployment_definition_id = $2, entrypoint_kind = 'actor',
       entrypoint_declared_id = 'test-actor', session_id = $3,
       cause_kind = 'actor_start', session_input_start_sequence = 1,
       session_input_high_watermark = 1, payload = NULL
 WHERE id = $1`, fixture.runID, actorDefinitionID, actorID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE run_attempts
   SET entrypoint_kind = 'actor', session_input_start_sequence = 1
 WHERE run_id = $1 AND number = 1`, fixture.runID)
	dbtest.MustExec(t, fixture.ctx, tx, `
UPDATE workspaces SET owner_run_id = NULL, owner_session_id = $2 WHERE id = $1`,
		fixture.workspaceID, actorID)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	return actorID
}
