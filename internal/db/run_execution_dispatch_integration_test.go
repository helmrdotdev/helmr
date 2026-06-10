package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLeaseRunExecutionSessionBindsWorkerInstanceDispatchLease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-a")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-a")

	sessionID := ids.ToPG(ids.New())
	leased, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-a"),
		DispatchLeaseID:   "lease-a",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if leased.SessionWorkerInstanceID != instance.ID {
		t.Fatalf("leased worker instance = %v, want %v", leased.SessionWorkerInstanceID, instance.ID)
	}
	if leased.SessionDispatchMessageID != "message-a" || leased.SessionDispatchLeaseID != "lease-a" || leased.SessionDispatchAttempt != 1 {
		t.Fatalf("leased redis lease fields = (%q, %q, %d)", leased.SessionDispatchMessageID, leased.SessionDispatchLeaseID, leased.SessionDispatchAttempt)
	}
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-a"),
		DispatchLeaseID:   "lease-b",
		DispatchAttempt:   2,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim error = %v, want no rows", err)
	}

	if status, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil || status != db.RunStatusRunning {
		t.Fatalf("start status = %q, err = %v", status, err)
	}
	startedVersion := requireRunStateVersion(t, ctx, pool, orgID, runID)
	if status, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil || status != db.RunStatusRunning {
		t.Fatalf("retry start status = %q, err = %v", status, err)
	}
	if got := requireRunStateVersion(t, ctx, pool, orgID, runID); got != startedVersion {
		t.Fatalf("state_version after retry start = %d, want %d", got, startedVersion)
	}
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "session.started", 1)
	if _, err := queries.AppendRunEvent(ctx, db.AppendRunEventParams{
		OrgID:   orgID,
		RunID:   runID,
		Kind:    "test.trigger_backfill",
		Payload: []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRunEventObservability(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, runID, "test.trigger_backfill", "system", "info", "control", "internal", false)
	if _, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("hello"),
		Kind:             "log",
		Payload:          []byte(`{"stream":"stdout"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("hello"),
		Kind:             "log",
		Payload:          []byte(`{"stream":"stdout"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRunEventObservability(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, runID, "log", "log", "info", "worker", "sensitive", true)
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "log_bytes", 1, int64(len("hello")))
	if _, err := queries.RenewRunQueueReservation(ctx, db.RenewRunQueueReservationParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    pgText("message-a"),
		ReservationExpiresAt: pgTime(time.Now().Add(2 * time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RenewRunExecutionSessionLease(ctx, db.RenewRunExecutionSessionLeaseParams{
		OrgID:             orgID,
		RunID:             runID,
		SessionID:         sessionID,
		WorkerInstanceID:  instance.ID,
		DispatchMessageID: "message-a",
		DispatchLeaseID:   "lease-a",
		LeaseExpiresAt:    pgTime(time.Now().Add(2 * time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	released, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                   orgID,
		RunID:                   runID,
		SessionID:               sessionID,
		WorkerInstanceID:        instance.ID,
		DispatchMessageID:       "message-a",
		DispatchLeaseID:         "lease-a",
		RunStatus:               db.RunStatusSucceeded,
		AttemptStatus:           db.RunAttemptStatusSucceeded,
		ExitCode:                pgtype.Int4{Int32: 0, Valid: true},
		Output:                  []byte(`{"ok":true}`),
		ReleaseActiveDurationMs: 1 << 60,
		TerminalEventKind:       "run.succeeded",
		TerminalEventPayload:    []byte(`{"exit_code":0}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != db.RunStatusSucceeded {
		t.Fatalf("released status = %q", released.Status)
	}
	requireRunEventObservability(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, runID, "run.succeeded", "lifecycle", "info", "control", "internal", true)
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "active_time", 1, int64(released.MaxDurationSeconds)*1000)
	requireRunUsageDuration(t, ctx, pool, orgID, runID, int64(released.MaxDurationSeconds)*1000)
	requireRunExecutionSessionActiveDuration(t, ctx, pool, orgID, runID, sessionID, int64(released.MaxDurationSeconds)*1000)
	requireRunUsageEventPositive(t, ctx, pool, orgID, runID, "output_bytes", 1)
	if _, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                   orgID,
		RunID:                   runID,
		SessionID:               sessionID,
		WorkerInstanceID:        instance.ID,
		DispatchMessageID:       "message-a",
		DispatchLeaseID:         "lease-a",
		RunStatus:               db.RunStatusSucceeded,
		AttemptStatus:           db.RunAttemptStatusSucceeded,
		ExitCode:                pgtype.Int4{Int32: 0, Valid: true},
		Output:                  []byte(`{"ok":true}`),
		ReleaseActiveDurationMs: 1 << 60,
		TerminalEventKind:       "run.succeeded",
		TerminalEventPayload:    []byte(`{"exit_code":0}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "active_time", 1, int64(released.MaxDurationSeconds)*1000)
	requireRunUsageEventPositive(t, ctx, pool, orgID, runID, "output_bytes", 1)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCompleted)
}

func TestReleaseRunExecutionSessionSchedulesRetryAttempt(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-retry")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := pool.Exec(ctx, `
UPDATE runs
   SET locked_retry_policy = '{"maxAttempts":3,"backoff":{"minMs":60000,"maxMs":60000,"jitter":"none"}}'::jsonb
 WHERE org_id = $1
   AND id = $2
`, orgID, runID); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-retry")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-retry"),
		DispatchLeaseID:   "lease-retry",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	released, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                   orgID,
		RunID:                   runID,
		SessionID:               sessionID,
		WorkerInstanceID:        instance.ID,
		DispatchMessageID:       "message-retry",
		DispatchLeaseID:         "lease-retry",
		RunStatus:               db.RunStatusFailed,
		AttemptStatus:           db.RunAttemptStatusFailed,
		ExitCode:                pgtype.Int4{Int32: 7, Valid: true},
		ReleaseActiveDurationMs: 1234,
		TerminalEventKind:       "run.failed",
		TerminalEventPayload:    []byte(`{"failure_kind":"task_failed","detail":{"exit_code":7}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != db.RunStatusQueued || released.ExecutionStatus != db.RunExecutionStatusQueued || released.TerminalOutcome.Valid {
		t.Fatalf("released retry state = status %s execution %s terminal %+v, want queued/queued/null", released.Status, released.ExecutionStatus, released.TerminalOutcome)
	}
	if !released.CurrentAttemptNumber.Valid || released.CurrentAttemptNumber.Int32 != 2 {
		t.Fatalf("current attempt number = %+v, want 2", released.CurrentAttemptNumber)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireCurrentRunAttemptStatus(t, ctx, pool, orgID, runID, db.RunAttemptStatusQueued)
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "run.retry_scheduled", 1)
	requireRunEventObservability(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, runID, "run.retry_scheduled", "lifecycle", "warn", "control", "internal", false)
	requireRunRetryDecision(t, ctx, pool, orgID, runID, sessionID, db.RunRetryDecisionKindRetry, "non_zero_exit", 2)
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "active_time", 1, 1234)
	requireRunUsageDuration(t, ctx, pool, orgID, runID, 1234)
	requireRunUsageEventSnapshotTransition(t, ctx, pool, orgID, runID, "active_time", "run.failed")

	candidates, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		QueueName:     "exec-queue",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("retry backoff candidates = %d, want 0", len(candidates))
	}

	idempotent, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                orgID,
		RunID:                runID,
		SessionID:            sessionID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    "message-retry",
		DispatchLeaseID:      "lease-retry",
		RunStatus:            db.RunStatusFailed,
		AttemptStatus:        db.RunAttemptStatusFailed,
		ExitCode:             pgtype.Int4{Int32: 7, Valid: true},
		TerminalEventKind:    "run.failed",
		TerminalEventPayload: []byte(`{"failure_kind":"task_failed","detail":{"exit_code":7}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if idempotent.Status != db.RunStatusQueued || !idempotent.CurrentAttemptNumber.Valid || idempotent.CurrentAttemptNumber.Int32 != 2 {
		t.Fatalf("idempotent retry release = status %s attempt %+v", idempotent.Status, idempotent.CurrentAttemptNumber)
	}
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "run.retry_scheduled", 1)
}

func TestGracefulCancelPendingRunFinalizesOnRelease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-graceful-cancel")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-graceful-cancel")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-graceful-cancel"),
		DispatchLeaseID:   "lease-graceful-cancel",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunID:         runID,
		Kind:          db.RunOperationKindCancel,
		ActorKind:     "test",
		ActorID:       "db-test",
		Reason:        "stop",
		Request:       []byte(`{"reason":"stop"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       orgID,
		RunID:       runID,
		Reason:      "stop",
		Force:       false,
		OperationID: operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != db.RunStatusCancelled || cancelled.ExecutionStatus != db.RunExecutionStatusPendingCancel {
		t.Fatalf("cancelled state = %s/%s, want cancelled/pending_cancel", cancelled.Status, cancelled.ExecutionStatus)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusReserved)
	if _, err := queries.RenewRunExecutionSessionLease(ctx, db.RenewRunExecutionSessionLeaseParams{
		OrgID:             orgID,
		RunID:             runID,
		SessionID:         sessionID,
		WorkerInstanceID:  instance.ID,
		DispatchMessageID: "message-graceful-cancel",
		DispatchLeaseID:   "lease-graceful-cancel",
		LeaseExpiresAt:    pgTime(time.Now().Add(2 * time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}

	released, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                orgID,
		RunID:                runID,
		SessionID:            sessionID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    "message-graceful-cancel",
		DispatchLeaseID:      "lease-graceful-cancel",
		RunStatus:            db.RunStatusSucceeded,
		AttemptStatus:        db.RunAttemptStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		Output:               []byte(`{"ignored":true}`),
		TerminalEventKind:    "run.succeeded",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != db.RunStatusCancelled || released.ExecutionStatus != db.RunExecutionStatusFinished || released.TerminalOutcome.RunTerminalOutcome != db.RunTerminalOutcomeCancelled {
		t.Fatalf("released cancelled state = %s/%s/%+v", released.Status, released.ExecutionStatus, released.TerminalOutcome)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCompleted)
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "run.cancel_requested", 1)
	requireRunEventObservability(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, runID, "run.cancelled", "lifecycle", "error", "control", "internal", true)
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "output_bytes", 0, 0)
	if idempotent, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                orgID,
		RunID:                runID,
		SessionID:            sessionID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    "message-graceful-cancel",
		DispatchLeaseID:      "lease-graceful-cancel",
		RunStatus:            db.RunStatusSucceeded,
		AttemptStatus:        db.RunAttemptStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		Output:               []byte(`{"ignored":true}`),
		TerminalEventKind:    "run.succeeded",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	}); err != nil {
		t.Fatal(err)
	} else if idempotent.Status != db.RunStatusCancelled || idempotent.ExecutionStatus != db.RunExecutionStatusFinished {
		t.Fatalf("idempotent release state = %s/%s, want cancelled/finished", idempotent.Status, idempotent.ExecutionStatus)
	}
}

func TestCancelLeasedRunBeforeStartFinalizesImmediately(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-cancel-leased")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-cancel-leased")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-cancel-leased"),
		DispatchLeaseID:   "lease-cancel-leased",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunID:         runID,
		Kind:          db.RunOperationKindCancel,
		ActorKind:     "test",
		ActorID:       "db-test",
		Reason:        "stop before start",
		Request:       []byte(`{"reason":"stop before start"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       orgID,
		RunID:       runID,
		Reason:      "stop before start",
		Force:       false,
		OperationID: operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != db.RunStatusCancelled || cancelled.ExecutionStatus != db.RunExecutionStatusFinished || cancelled.TerminalOutcome.RunTerminalOutcome != db.RunTerminalOutcomeCancelled {
		t.Fatalf("cancelled leased state = %s/%s/%+v, want cancelled/finished/cancelled", cancelled.Status, cancelled.ExecutionStatus, cancelled.TerminalOutcome)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCancelled)
	requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessionID, db.RunExecutionSessionStatusCancelled)
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "run.cancelled", 1)
}

func TestForceCancelActiveRunFinalizesImmediately(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-force-cancel")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-force-cancel")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-force-cancel"),
		DispatchLeaseID:   "lease-force-cancel",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunID:         runID,
		Kind:          db.RunOperationKindCancel,
		ActorKind:     "test",
		ActorID:       "db-test",
		Reason:        "force stop",
		Request:       []byte(`{"force":true,"reason":"force stop"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       orgID,
		RunID:       runID,
		Reason:      "force stop",
		Force:       true,
		OperationID: operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != db.RunStatusCancelled || cancelled.ExecutionStatus != db.RunExecutionStatusFinished || cancelled.TerminalOutcome.RunTerminalOutcome != db.RunTerminalOutcomeCancelled {
		t.Fatalf("force cancelled state = %s/%s/%+v", cancelled.Status, cancelled.ExecutionStatus, cancelled.TerminalOutcome)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCancelled)
	requireCurrentRunAttemptStatus(t, ctx, pool, orgID, runID, db.RunAttemptStatusCancelled)
	requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessionID, db.RunExecutionSessionStatusCancelled)
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "run.cancelled", 1)
	requireRunEventObservability(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, runID, "run.cancelled", "lifecycle", "warn", "control", "internal", false)
}

func TestLeaseRunExecutionSessionRequiresMatchingWorkerGroup(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-worker-group-a")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-worker-group")
	secondWorkerGroupID := createPostgresTestWorkerGroup(t, ctx, pool, "lease-secondary")
	if _, err := pool.Exec(ctx, `
UPDATE run_runtime_requirements
   SET worker_group_id = $1
 WHERE org_id = $2
   AND run_id = $3
`, secondWorkerGroupID, orgID, runID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-worker-group"),
		DispatchLeaseID:   "lease-worker-group",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lease error = %v, want no rows", err)
	}
}

func TestLeaseRunExecutionSessionSeparatesWorkerGroupsWithinSharedQueue(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instanceA := upsertTestWorkerInstance(t, ctx, queries, "runner-shared-queue-a")
	workerGroupB := createPostgresTestWorkerGroup(t, ctx, pool, "lease-shared-queue-b")
	instanceB := upsertTestWorkerInstanceInGroup(t, ctx, queries, "runner-shared-queue-b", workerGroupB)
	runA := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	runB := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runA, "shared-queue", instanceA, "message-shared-a")
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runB, "shared-queue", instanceB, "message-shared-b")

	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runB,
		WorkerInstanceID:  instanceA.ID,
		SessionID:         ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-shared-b"),
		DispatchLeaseID:   "lease-shared-b",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-group lease error = %v, want no rows", err)
	}
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runA,
		WorkerInstanceID:  instanceA.ID,
		SessionID:         ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-shared-a"),
		DispatchLeaseID:   "lease-shared-a",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseRunExecutionSessionHonorsQueuedExpiry(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-expired-queued")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := pool.Exec(ctx, `UPDATE runs SET queued_expires_at = now() - interval '1 second' WHERE org_id = $1 AND id = $2`, orgID, runID); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-expired")

	_, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-expired"),
		DispatchLeaseID:   "lease-expired",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lease error = %v, want no rows", err)
	}
}

func TestLeaseRunExecutionSessionHonorsQueueConcurrencyLimit(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-limited-queue")
	firstRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	secondRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := pool.Exec(ctx, `
UPDATE runs
   SET queue_name = 'limited',
       queue_concurrency_limit = 1
 WHERE org_id = $1
   AND id IN ($2, $3)
`, orgID, firstRunID, secondRunID); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, firstRunID, "limited", instance, "message-limited-a")
	seedLeasableRunQueueItem(t, ctx, queries, orgID, secondRunID, "limited", instance, "message-limited-b")

	type leaseAttempt struct {
		runID     pgtype.UUID
		execID    pgtype.UUID
		messageID string
		leaseID   string
	}
	type leaseResult struct {
		attempt leaseAttempt
		err     error
	}
	attempts := []leaseAttempt{{
		runID:     firstRunID,
		execID:    ids.ToPG(ids.New()),
		messageID: "message-limited-a",
		leaseID:   "lease-limited-a",
	}, {
		runID:     secondRunID,
		execID:    ids.ToPG(ids.New()),
		messageID: "message-limited-b",
		leaseID:   "lease-limited-b",
	}}
	start := make(chan struct{})
	results := make(chan leaseResult, len(attempts))
	var wg sync.WaitGroup
	for _, attempt := range attempts {
		wg.Go(func() {
			<-start
			_, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
				OrgID:             orgID,
				RunID:             attempt.runID,
				WorkerInstanceID:  instance.ID,
				SessionID:         attempt.execID,
				DispatchMessageID: pgText(attempt.messageID),
				DispatchLeaseID:   attempt.leaseID,
				DispatchAttempt:   1,
				LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
				SessionSpanID:     "0123456789abcdef",
			})
			results <- leaseResult{attempt: attempt, err: err}
		})
	}
	close(start)
	wg.Wait()
	close(results)

	var leased leaseAttempt
	var blocked leaseAttempt
	var leasedCount int
	var blockedCount int
	for result := range results {
		switch {
		case result.err == nil:
			leased = result.attempt
			leasedCount++
		case errors.Is(result.err, pgx.ErrNoRows):
			blocked = result.attempt
			blockedCount++
		default:
			t.Fatalf("lease error = %v", result.err)
		}
	}
	if leasedCount != 1 || blockedCount != 1 {
		t.Fatalf("leased=%d blocked=%d, want one lease and one blocked", leasedCount, blockedCount)
	}
	requireActiveConcurrencySlot(t, ctx, pool, orgID, leased.runID, leased.execID)
	requireNoActiveConcurrencySlot(t, ctx, pool, orgID, blocked.runID, blocked.execID)

	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            leased.runID,
		SessionID:        leased.execID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                orgID,
		RunID:                leased.runID,
		SessionID:            leased.execID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    leased.messageID,
		DispatchLeaseID:      leased.leaseID,
		RunStatus:            db.RunStatusSucceeded,
		AttemptStatus:        db.RunAttemptStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		TerminalEventKind:    "run.succeeded",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             blocked.runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         ids.ToPG(ids.New()),
		DispatchMessageID: pgText(blocked.messageID),
		DispatchLeaseID:   blocked.leaseID,
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRequeueExpiredLeasedRunExecutionSessionRestoresDispatchContract(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-expired-leased")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	queuedExpiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
	UPDATE runs
	   SET queue_name = 'limited-expired-leased',
	       queue_concurrency_limit = 1,
	       concurrency_key = 'same-key',
	       ttl = '1h',
	       queued_expires_at = $3
	 WHERE org_id = $1
	   AND id = $2
	`, orgID, runID, queuedExpiresAt); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "limited-expired-leased", instance, "message-expired-leased")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-expired-leased"),
		DispatchLeaseID:   "lease-expired-leased",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	queueItemBeforeRequeue := requireRunQueueItemDispatchState(t, ctx, pool, orgID, runID)
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusRestoring)
	if _, err := pool.Exec(ctx, `
	UPDATE run_execution_sessions
   SET lease_expires_at = now() - interval '1 second'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID); err != nil {
		t.Fatal(err)
	}

	if err := queries.RequeueExpiredLeasedRunExecutionSessions(ctx, orgID); err != nil {
		t.Fatal(err)
	}

	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	runAfterRequeue, err := queries.GetRun(ctx, db.GetRunParams{OrgID: orgID, ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if !runAfterRequeue.QueuedExpiresAt.Valid || !runAfterRequeue.QueuedExpiresAt.Time.Equal(queuedExpiresAt) {
		t.Fatalf("run queued expiry = %+v, want %s", runAfterRequeue.QueuedExpiresAt, queuedExpiresAt)
	}
	queueItemAfterRequeue := requireRunQueueItemDispatchState(t, ctx, pool, orgID, runID)
	if queueItemAfterRequeue.Status != db.RunQueueStatusQueued {
		t.Fatalf("run queue status = %s, want %s", queueItemAfterRequeue.Status, db.RunQueueStatusQueued)
	}
	if !queueItemAfterRequeue.QueuedExpiresAt.Valid || !queueItemAfterRequeue.QueuedExpiresAt.Time.Equal(queuedExpiresAt) {
		t.Fatalf("run queue queued expiry = %+v, want %s", queueItemAfterRequeue.QueuedExpiresAt, queuedExpiresAt)
	}
	if queueItemAfterRequeue.DispatchGeneration != queueItemBeforeRequeue.DispatchGeneration+1 {
		t.Fatalf("dispatch generation = %d, want %d", queueItemAfterRequeue.DispatchGeneration, queueItemBeforeRequeue.DispatchGeneration+1)
	}
	if queueItemAfterRequeue.DispatchMessageID.Valid ||
		queueItemAfterRequeue.ReservedByWorkerInstanceID.Valid ||
		queueItemAfterRequeue.ReservationExpiresAt.Valid {
		t.Fatalf("queue reservation fields after requeue = %+v", queueItemAfterRequeue)
	}
	if queueItemAfterRequeue.LastError != "worker lease expired before execution started" {
		t.Fatalf("queue last error = %q", queueItemAfterRequeue.LastError)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
	requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessionID, db.RunExecutionSessionStatusLost)
	requireNoActiveConcurrencySlot(t, ctx, pool, orgID, runID, sessionID)
	requireRunSessionEvent(t, ctx, pool, orgID, runID, sessionID, int32(1), "run.execution_lost", []byte(`{"reason":"worker lease expired before execution started","source":"lease_sweeper"}`))
	requireNoRunEventKind(t, ctx, pool, orgID, runID, "run.failed")

	candidates, err := queries.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		QueueName:     "limited-expired-leased",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RunID != runID || candidates[0].DispatchMessageID != "" {
		t.Fatalf("queue candidates = %+v", candidates)
	}
}

func TestCancelRequeuedLeasedRunFinalizesQueueItem(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-requeue-cancel")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-requeue-cancel")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-requeue-cancel"),
		DispatchLeaseID:   "lease-requeue-cancel",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE run_execution_sessions
   SET lease_expires_at = now() - interval '1 second'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := queries.RequeueExpiredLeasedRunExecutionSessions(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	requeued, err := queries.GetRun(ctx, db.GetRunParams{OrgID: orgID, ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != db.RunStatusQueued || requeued.ExecutionStatus != db.RunExecutionStatusQueued {
		t.Fatalf("requeued state = %s/%s, want queued/queued", requeued.Status, requeued.ExecutionStatus)
	}

	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunID:         runID,
		Kind:          db.RunOperationKindCancel,
		ActorKind:     "test",
		ActorID:       "db-test",
		Reason:        "stop",
		Request:       []byte(`{"reason":"stop"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       orgID,
		RunID:       runID,
		Reason:      "stop",
		Force:       false,
		OperationID: operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != db.RunStatusCancelled || cancelled.ExecutionStatus != db.RunExecutionStatusFinished || cancelled.TerminalOutcome.RunTerminalOutcome != db.RunTerminalOutcomeCancelled {
		t.Fatalf("cancelled requeued state = %s/%s/%+v, want cancelled/finished/cancelled", cancelled.Status, cancelled.ExecutionStatus, cancelled.TerminalOutcome)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCancelled)
}

func TestRequeueExpiredLeasedRunExecutionSessionsHandlesMultipleRuns(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-expired-leased-multi")
	runs := make([]pgtype.UUID, 0, 2)
	sessions := make([]pgtype.UUID, 0, 2)
	for _, suffix := range []string{"a", "b"} {
		runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
		messageID := "message-expired-leased-multi-" + suffix
		seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "multi-expired-leased", instance, messageID)
		sessionID := ids.ToPG(ids.New())
		if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
			OrgID:             orgID,
			RunID:             runID,
			WorkerInstanceID:  instance.ID,
			SessionID:         sessionID,
			DispatchMessageID: pgText(messageID),
			DispatchLeaseID:   "lease-expired-leased-multi-" + suffix,
			DispatchAttempt:   1,
			LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
			SessionSpanID:     "0123456789abcdef",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
UPDATE run_execution_sessions
   SET lease_expires_at = now() - interval '1 second'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, runID)
		sessions = append(sessions, sessionID)
	}

	if err := queries.RequeueExpiredLeasedRunExecutionSessions(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	for i, runID := range runs {
		requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
		requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessions[i], db.RunExecutionSessionStatusLost)
		requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "session.lost_requeued", 1)
		requireRunEventKindCount(t, ctx, pool, orgID, runID, "run.execution_lost", 1)
	}
}

func TestFailExpiredRunningRunExecutionSessionsSweepsOpeningWaitpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-expired-opening")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-opening")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-opening"),
		DispatchLeaseID:   "lease-opening",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "wait-expired-opening",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	UPDATE run_execution_sessions
	   SET lease_expires_at = now() - interval '1 second'
	 WHERE org_id = $1
	   AND run_id = $2
	   AND id = $3
	`, orgID, runID, sessionID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunExecutionSessions(ctx, orgID); err != nil {
		t.Fatal(err)
	}

	run, err := queries.GetRun(ctx, db.GetRunParams{OrgID: orgID, ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != db.RunStatusFailed || run.CurrentSessionID.Valid || run.ErrorMessage.String != "worker lease expired" {
		t.Fatalf("run after sweep = %+v", run)
	}
	requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessionID, db.RunExecutionSessionStatusLost)
	var waitpointStatus db.WaitpointStatus
	var resolutionKind pgtype.Text
	if err := pool.QueryRow(ctx, `
	SELECT waitpoints.status, waitpoints.resolution_kind
	  FROM waitpoints
	  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waitpoints.org_id
	                            AND run_wait_dependencies.waitpoint_id = waitpoints.id
	 WHERE waitpoints.org_id = $1
	   AND run_wait_dependencies.run_id = $2
	   AND waitpoints.id = $3
	`, orgID, runID, waitpointID).Scan(&waitpointStatus, &resolutionKind); err != nil {
		t.Fatal(err)
	}
	if waitpointStatus != db.WaitpointStatusCancelled || resolutionKind.String != "cancelled" {
		t.Fatalf("waitpoint status = %s resolution = %+v", waitpointStatus, resolutionKind)
	}
	requireCancelledWaitpointPayloads(t, ctx, pool, orgID, runID, waitpointID, []byte(`{"reason":"worker lease expired","source":"lease_sweeper"}`))
	var checkpointStatus db.CheckpointStatus
	if err := pool.QueryRow(ctx, `
	SELECT status
	  FROM checkpoints
	 WHERE org_id = $1
	   AND run_id = $2
	   AND id = $3
	`, orgID, runID, checkpointID).Scan(&checkpointStatus); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusInvalid {
		t.Fatalf("checkpoint status = %s", checkpointStatus)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCompleted)
	requireRunSessionEvent(t, ctx, pool, orgID, runID, sessionID, int32(1), "run.execution_lost", []byte(`{"reason":"worker lease expired","source":"lease_sweeper"}`))
	requireRunSessionEvent(t, ctx, pool, orgID, runID, sessionID, int32(1), "run.failed", []byte(`{"failure_kind":"worker_lease_expired","detail":{"message":"worker lease expired"}}`))
}

func TestFailExpiredRunningRunExecutionSessionsSchedulesInfraLostRetry(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-expired-retry")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := pool.Exec(ctx, `
UPDATE runs
   SET locked_retry_policy = '{"maxAttempts":3,"backoff":{"minMs":60000,"maxMs":60000,"jitter":"none"}}'::jsonb
 WHERE org_id = $1
   AND id = $2
`, orgID, runID); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-expired-retry")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-expired-retry"),
		DispatchLeaseID:   "lease-expired-retry",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE run_execution_sessions
   SET lease_expires_at = now() - interval '1 second'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunExecutionSessions(ctx, orgID); err != nil {
		t.Fatal(err)
	}

	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireCurrentRunAttemptStatus(t, ctx, pool, orgID, runID, db.RunAttemptStatusQueued)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessionID, db.RunExecutionSessionStatusLost)
	requireRunRetryDecision(t, ctx, pool, orgID, runID, sessionID, db.RunRetryDecisionKindRetry, "infra_lost", 2)
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "session.lost_failed", 1)
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "run.retry_scheduled", 1)
	requireRunEventKindCount(t, ctx, pool, orgID, runID, "run.retry_scheduled", 1)
}

func TestFailExpiredRunningRunExecutionSessionsHandlesMultipleRuns(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-expired-running-multi")
	runs := make([]pgtype.UUID, 0, 2)
	sessions := make([]pgtype.UUID, 0, 2)
	for _, suffix := range []string{"a", "b"} {
		runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
		messageID := "message-expired-running-multi-" + suffix
		seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "multi-expired-running", instance, messageID)
		sessionID := ids.ToPG(ids.New())
		if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
			OrgID:             orgID,
			RunID:             runID,
			WorkerInstanceID:  instance.ID,
			SessionID:         sessionID,
			DispatchMessageID: pgText(messageID),
			DispatchLeaseID:   "lease-expired-running-multi-" + suffix,
			DispatchAttempt:   1,
			LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
			SessionSpanID:     "0123456789abcdef",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
			OrgID:            orgID,
			RunID:            runID,
			SessionID:        sessionID,
			WorkerInstanceID: instance.ID,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
UPDATE run_execution_sessions
   SET lease_expires_at = now() - interval '1 second'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, runID)
		sessions = append(sessions, sessionID)
	}

	if err := queries.FailExpiredRunningRunExecutionSessions(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	for i, runID := range runs {
		requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusFailed)
		requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessions[i], db.RunExecutionSessionStatusLost)
		requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "session.lost_failed", 1)
		requireRunEventKindCount(t, ctx, pool, orgID, runID, "run.failed", 1)
		requireRunEventKindCount(t, ctx, pool, orgID, runID, "run.execution_lost", 1)
	}
}

func TestGracefulCancelPendingRunFinalizesWhenSessionLeaseExpires(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-graceful-cancel-expired")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := pool.Exec(ctx, `
UPDATE runs
   SET queue_concurrency_limit = 1
 WHERE org_id = $1
   AND id = $2
`, orgID, runID); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-graceful-cancel-expired")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-graceful-cancel-expired"),
		DispatchLeaseID:   "lease-graceful-cancel-expired",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunID:         runID,
		Kind:          db.RunOperationKindCancel,
		ActorKind:     "test",
		ActorID:       "db-test",
		Reason:        "stop",
		Request:       []byte(`{"reason":"stop"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       orgID,
		RunID:       runID,
		Reason:      "stop",
		Force:       false,
		OperationID: operation.ID,
	}); err != nil {
		t.Fatal(err)
	}
	requireActiveConcurrencySlot(t, ctx, pool, orgID, runID, sessionID)
	if _, err := pool.Exec(ctx, `
UPDATE run_execution_sessions
   SET lease_expires_at = now() - interval '1 second'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID); err != nil {
		t.Fatal(err)
	}

	if err := queries.FailExpiredRunningRunExecutionSessions(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusCancelled)
	requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessionID, db.RunExecutionSessionStatusLost)
	requireNoActiveConcurrencySlot(t, ctx, pool, orgID, runID, sessionID)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCompleted)
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "session.lost_cancelled", 1)
	requireRunEventObservability(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, runID, "run.cancelled", "lifecycle", "warn", "lease_sweeper", "internal", true)
}

func TestForceCancelEscalatesPendingCancelRun(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-force-cancel-pending")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	if _, err := pool.Exec(ctx, `
UPDATE runs
   SET queue_concurrency_limit = 1
 WHERE org_id = $1
   AND id = $2
`, orgID, runID); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-force-cancel-pending")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-force-cancel-pending"),
		DispatchLeaseID:   "lease-force-cancel-pending",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	gracefulOperation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunID:         runID,
		Kind:          db.RunOperationKindCancel,
		ActorKind:     "test",
		ActorID:       "db-test",
		Reason:        "stop",
		Request:       []byte(`{"reason":"stop"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       orgID,
		RunID:       runID,
		Reason:      "stop",
		Force:       false,
		OperationID: gracefulOperation.ID,
	}); err != nil {
		t.Fatal(err)
	}
	requireActiveConcurrencySlot(t, ctx, pool, orgID, runID, sessionID)
	forceOperation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunID:         runID,
		Kind:          db.RunOperationKindCancel,
		ActorKind:     "test",
		ActorID:       "db-test",
		Reason:        "force",
		Request:       []byte(`{"reason":"force","force":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	cancelled, err := queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       orgID,
		RunID:       runID,
		Reason:      "force",
		Force:       true,
		OperationID: forceOperation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != db.RunStatusCancelled || cancelled.ExecutionStatus != db.RunExecutionStatusFinished || cancelled.TerminalOutcome.RunTerminalOutcome != db.RunTerminalOutcomeCancelled {
		t.Fatalf("force cancelled state = %s/%s/%+v", cancelled.Status, cancelled.ExecutionStatus, cancelled.TerminalOutcome)
	}
	requireRunExecutionSessionStatus(t, ctx, pool, orgID, runID, sessionID, db.RunExecutionSessionStatusCancelled)
	requireNoActiveConcurrencySlot(t, ctx, pool, orgID, runID, sessionID)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusCancelled)
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "run.cancelled", 1)
}

func TestReleaseRunExecutionSessionSeparatesCancelledWaitpointOutputAndResolution(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-release-cancelled-waitpoint")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	messageID := "message-release-cancelled-waitpoint"
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, messageID)
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText(messageID),
		DispatchLeaseID:   "lease-release-cancelled-waitpoint",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "release-cancelled-waitpoint",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE run_execution_sessions
   SET started_at = now() - interval '2 seconds'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                orgID,
		RunID:                runID,
		SessionID:            sessionID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    messageID,
		DispatchLeaseID:      "lease-release-cancelled-waitpoint",
		RunStatus:            db.RunStatusFailed,
		AttemptStatus:        db.RunAttemptStatusFailed,
		ErrorMessage:         pgText("worker failed"),
		TerminalEventKind:    "run.failed",
		TerminalEventPayload: []byte(`{"failure_kind":"worker_failed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRunEventObservability(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID, runID, "run.failed", "lifecycle", "error", "control", "internal", true)
	requireRunUsageEventPositive(t, ctx, pool, orgID, runID, "active_time", 1)
	requireCancelledWaitpointPayloads(t, ctx, pool, orgID, runID, waitpointID, []byte(`{"reason":"worker failed","source":"release"}`))
}

func TestCreateWaitpointForExecutionRequiresRunningExecution(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-leased-waitpoint")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-leased-waitpoint")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-leased-waitpoint"),
		DispatchLeaseID:   "lease-leased-waitpoint",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}

	_, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "wait-before-start",
		CheckpointID:     ids.ToPG(ids.New()),
		CheckpointReason: "waitpoint",
		RunWaitID:        ids.ToPG(ids.New()),
		ID:               ids.ToPG(ids.New()),
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("create waitpoint error = %v, want no rows", err)
	}
}

func TestMarkWaitpointCheckpointDurableReadyCompletesRestoredCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-restored-next-waitpoint")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-restored-next")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-restored-next"),
		DispatchLeaseID:   "lease-restored-next",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusRestoring)
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	restoreRunWaitID, restoreWaitpointID := requireWaitpointForCheckpoint(t, ctx, pool, orgID, runID, restoreCheckpointID)
	if _, err := queries.AcknowledgeRestore(ctx, db.AcknowledgeRestoreParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CheckpointID:     restoreCheckpointID,
		RunWaitID:        restoreRunWaitID,
		WaitpointID:      restoreWaitpointID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AcknowledgeRestore(ctx, db.AcknowledgeRestoreParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CheckpointID:     restoreCheckpointID,
		RunWaitID:        restoreRunWaitID,
		WaitpointID:      restoreWaitpointID,
	}); err != nil {
		t.Fatalf("second restore acknowledgement: %v", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
	requireWaitpointStatus(t, ctx, pool, orgID, runID, restoreWaitpointID, db.RunWaitStatusRestored)
	nextCheckpointID := ids.ToPG(ids.New())
	nextRunWaitID := ids.ToPG(ids.New())
	nextWaitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "next-waitpoint",
		CheckpointID:     nextCheckpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        nextRunWaitID,
		ID:               nextWaitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	UPDATE run_queue_items
	   SET reservation_expires_at = now() - interval '1 second'
	 WHERE org_id = $1
	   AND run_id = $2
	   AND reserved_by_worker_instance_id = $3
	   AND dispatch_message_id = 'message-restored-next'
	`, orgID, runID, instance.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		SessionID:                  sessionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  nextRunWaitID,
		WaitpointID:                nextWaitpointID,
		CheckpointID:               nextCheckpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             "firecracker",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           10000,
		CheckpointPayload:          []byte(`{"checkpoint_id":"next"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRuntimeConfigArtifact(t, ctx, pool, orgID, runID, nextCheckpointID)
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "active_time", 1, 10000)
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "checkpoint_bytes", 1, 10)
	requireRunUsageDuration(t, ctx, pool, orgID, runID, 10000)
	requireRunUsageEventSnapshotTransition(t, ctx, pool, orgID, runID, "active_time", "checkpoint.ready")
	requireRunUsageEventSnapshotTransition(t, ctx, pool, orgID, runID, "checkpoint_bytes", "checkpoint.ready")

	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		OrgID:          orgID,
		ID:             nextWaitpointID,
		Kind:           db.WaitpointKindHuman,
		ResolutionKind: pgText("completed"),
		Output:         []byte(`{"approved":true}`),
		Resolution:     approvedWaitpointResolution("admin"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: nextWaitpointID}); err != nil {
		t.Fatal(err)
	}
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-restored-final")
	resumedSessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         resumedSessionID,
		DispatchMessageID: pgText("message-restored-final"),
		DispatchLeaseID:   "lease-restored-final",
		DispatchAttempt:   2,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        resumedSessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE run_execution_sessions
   SET started_at = now() - interval '2 seconds'
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, resumedSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                   orgID,
		RunID:                   runID,
		SessionID:               resumedSessionID,
		WorkerInstanceID:        instance.ID,
		DispatchMessageID:       "message-restored-final",
		DispatchLeaseID:         "lease-restored-final",
		RunStatus:               db.RunStatusFailed,
		AttemptStatus:           db.RunAttemptStatusFailed,
		ErrorMessage:            pgText("worker failed after resume"),
		ReleaseActiveDurationMs: 0,
		TerminalEventKind:       "run.failed",
		TerminalEventPayload:    []byte(`{"failure_kind":"worker_failed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireRunUsageEventAtLeast(t, ctx, pool, orgID, runID, "active_time", 2, 11000)
}

func TestMarkWaitpointCheckpointDurableReadyRequiresLeaseRuntime(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-checkpoint-runtime")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-checkpoint-runtime")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-checkpoint-runtime"),
		DispatchLeaseID:   "lease-checkpoint-runtime",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runWaitID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "checkpoint-runtime",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		SessionID:                  sessionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  runWaitID,
		WaitpointID:                waitpointID,
		CheckpointID:               checkpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             "firecracker",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:other-kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"mismatch"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("checkpoint ready runtime mismatch error = %v, want no rows", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, checkpointID, db.CheckpointStatusCreating)
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusOpening)
	requireNoCheckpointArtifacts(t, ctx, pool, orgID, runID, checkpointID)
}

func TestMarkWaitpointCheckpointDurableReadyRejectsUnsupportedRuntimeBackend(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-checkpoint-backend")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-checkpoint-backend")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-checkpoint-backend"),
		DispatchLeaseID:   "lease-checkpoint-backend",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runWaitID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "checkpoint-backend",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		SessionID:                  sessionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  runWaitID,
		WaitpointID:                waitpointID,
		CheckpointID:               checkpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"test"}}`),
		RuntimeBackend:             "test",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"unsupported-backend"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("checkpoint ready backend error = %v, want no rows", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, checkpointID, db.CheckpointStatusCreating)
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusOpening)
	requireNoCheckpointArtifacts(t, ctx, pool, orgID, runID, checkpointID)
}

func TestMarkWaitpointCheckpointFailedSeparatesOutputAndResolution(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-checkpoint-failed")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	messageID := "message-checkpoint-failed"
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, messageID)
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText(messageID),
		DispatchLeaseID:   "lease-checkpoint-failed",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "checkpoint-failed",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err := queries.MarkWaitpointCheckpointFailed(ctx, db.MarkWaitpointCheckpointFailedParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		RunWaitID:        runWaitID,
		WaitpointID:      waitpointID,
		CheckpointID:     checkpointID,
		ErrorMessage:     pgText("snapshot upload failed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != db.RunWaitStatusFailed || resolved.ResolutionKind.String != "cancelled" {
		t.Fatalf("resolved waitpoint = %+v", resolved)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, checkpointID, db.CheckpointStatusInvalid)
	requireCancelledWaitpointPayloads(t, ctx, pool, orgID, runID, waitpointID, []byte(`{"reason":"snapshot upload failed","source":"checkpoint"}`))
}

func TestLeaseRunExecutionSessionRequiresRestoreRuntimeSnapshot(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-restore-missing-runtime")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-missing-runtime")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	if _, err := pool.Exec(ctx, `
	DELETE FROM checkpoint_runtime_snapshots
	 WHERE org_id = $1
	   AND run_id = $2
	   AND checkpoint_id = $3
	`, orgID, runID, restoreCheckpointID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         ids.ToPG(ids.New()),
		DispatchMessageID: pgText("message-missing-runtime"),
		DispatchLeaseID:   "lease-missing-runtime",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lease error = %v, want no rows", err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusReady)
}

func TestRespondWaitpointResponseTokenResolvesSingleResponse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "token-single-response")
	tokenID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
INSERT INTO waitpoint_response_tokens (id, org_id, project_id, environment_id, waitpoint_id, token_hash, expires_at, external_subject, metadata)
SELECT $1, $2, waitpoints.project_id, waitpoints.environment_id, $4, '\x01', now() + interval '5 minutes', 'reviewer@example.com', '{}'
  FROM run_wait_dependencies
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
 WHERE run_wait_dependencies.org_id = $2
   AND run_wait_dependencies.run_id = $3
   AND run_wait_dependencies.waitpoint_id = $4
`, tokenID, orgID, runID, waitpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointResponseTokenCompleted(ctx, db.MarkWaitpointResponseTokenCompletedParams{
		OrgID:                orgID,
		ID:                   tokenID,
		TokenHash:            []byte{1},
		CompletedByPrincipal: pgText("reviewer@example.com"),
		CompletedVia:         pgText("email_token"),
		Metadata:             []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:             ids.ToPG(ids.New()),
		OrgID:          orgID,
		WaitpointID:    waitpointID,
		ResponseKey:    "email:reviewer@example.com",
		RequestHash:    "same",
		Action:         "respond",
		Kind:           db.WaitpointKindHuman,
		ResolutionKind: pgText("completed"),
		Resolution:     approvedWaitpointResolution("reviewer@example.com"),
		EventPayload:   []byte(`{"resolution_kind":"completed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, runID, waitpointID)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID}); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireWaitpointResponseCount(t, ctx, pool, orgID, runID, waitpointID, 1)
	requireWaitpointCompletionPayloads(t, ctx, pool, orgID, runID, waitpointID, []byte(`{"approved":true}`), approvedWaitpointResolution("reviewer@example.com"))
	requireRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestRespondBeforeRunWaitUnblocksAfterCheckpointReady(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)

	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateHumanWaitpoint(ctx, db.CreateHumanWaitpointParams{
		ID:                    waitpointID,
		OrgID:                 orgID,
		ProjectID:             scope.ProjectID,
		EnvironmentID:         scope.EnvironmentID,
		Request:               []byte(`{"message":"approve"}`),
		DisplayText:           "approve",
		ExpiresAt:             pgTime(time.Now().Add(time.Hour)),
		IdempotencyKeyOptions: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		ResponseKey:          "api:owner",
		RequestHash:          "same-request",
		Action:               "respond",
		Kind:                 db.WaitpointKindHuman,
		ResolutionKind:       pgText("completed"),
		Resolution:           approvedWaitpointResolution("owner@example.com"),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText("owner@example.com"),
		CompletedVia:         pgText("authenticated_api"),
		Metadata:             []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, pgtype.UUID{}, waitpointID)); err != nil {
		t.Fatal(err)
	}
	if resumed, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID}); err != nil || len(resumed) != 0 {
		t.Fatalf("pre-wait unblock = %+v, err = %v", resumed, err)
	}

	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-pre-respond")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-pre-respond")
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-pre-respond"),
		DispatchLeaseID:   "lease-pre-respond",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runWaitID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    "pre-respond",
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		SessionID:                  sessionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  runWaitID,
		WaitpointID:                waitpointID,
		CheckpointID:               checkpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             "firecracker",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("7")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"pre-respond"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusWaiting)

	resumed, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0].RunWaitID != runWaitID || resumed[0].Status != db.RunWaitStatusResuming {
		t.Fatalf("resumed = %+v", resumed)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
}

func TestResolveWaitpointRecordsAndResolvesSingleResponse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "api-single-response")
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		ResponseKey:          "user:admin",
		Action:               "respond",
		ResolutionKind:       pgText("completed"),
		Resolution:           approvedWaitpointResolution("admin"),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText("admin"),
		CompletedVia:         pgText("authenticated_api"),
		Metadata:             []byte(`{}`),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		Kind:                 db.WaitpointKindHuman,
		RequestHash:          "same",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		OrgID:          orgID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindHuman,
		ResolutionKind: pgText("completed"),
		Output:         []byte(`{"approved":true}`),
		Resolution:     approvedWaitpointResolution("admin"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID}); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireWaitpointResponseCount(t, ctx, pool, orgID, runID, waitpointID, 1)
	requireRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestResolveWaitpointRequiresSuspendedQueueEntryBeforeMutating(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "api-missing-suspended")
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		ResponseKey:          "user:admin",
		Action:               "respond",
		ResolutionKind:       pgText("completed"),
		Resolution:           []byte(`{"value":{"approved":true}}`),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText("admin"),
		CompletedVia:         pgText("authenticated_api"),
		Metadata:             []byte(`{}`),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		Kind:                 db.WaitpointKindHuman,
		RequestHash:          "same",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE run_queue_items
   SET status = 'queued'
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, runID, waitpointID)); err != nil {
		t.Fatal(err)
	}
	resumed, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 0 {
		t.Fatalf("resumed = %d, want 0", len(resumed))
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusWaiting)
	requireWaitpointConditionStatus(t, ctx, pool, orgID, waitpointID, db.WaitpointStatusCompleted)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusWaiting)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireNoRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestExpireDuePendingWaitpointsMarksConditionExpired(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "timeout-expired")
	if _, err := pool.Exec(ctx, `
UPDATE run_waits
   SET timeout_seconds = 1,
       waiting_at = now() - interval '2 seconds'
  FROM run_wait_dependencies
 WHERE run_waits.org_id = $1
   AND run_waits.run_id = $2
   AND run_wait_dependencies.org_id = run_waits.org_id
   AND run_wait_dependencies.run_wait_id = run_waits.id
   AND run_wait_dependencies.waitpoint_id = $3
`, orgID, runID, waitpointID); err != nil {
		t.Fatal(err)
	}

	if err := queries.ExpireDuePendingWaitpoints(ctx, orgID); err != nil {
		t.Fatal(err)
	}

	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireWaitpointConditionStatus(t, ctx, pool, orgID, waitpointID, db.WaitpointStatusExpired)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestExpireDuePendingWaitpointsHandlesMultipleRuns(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	type waitingRun struct {
		runID       pgtype.UUID
		waitpointID pgtype.UUID
	}
	waitingRuns := make([]waitingRun, 0, 2)
	for _, suffix := range []string{"timeout-expired-multi-a", "timeout-expired-multi-b"} {
		runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, suffix)
		if _, err := pool.Exec(ctx, `
UPDATE run_waits
   SET timeout_seconds = 1,
       waiting_at = now() - interval '2 seconds'
  FROM run_wait_dependencies
 WHERE run_waits.org_id = $1
   AND run_waits.run_id = $2
   AND run_wait_dependencies.org_id = run_waits.org_id
   AND run_wait_dependencies.run_wait_id = run_waits.id
   AND run_wait_dependencies.waitpoint_id = $3
`, orgID, runID, waitpointID); err != nil {
			t.Fatal(err)
		}
		waitingRuns = append(waitingRuns, waitingRun{runID: runID, waitpointID: waitpointID})
	}

	if err := queries.ExpireDuePendingWaitpoints(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	for _, waiting := range waitingRuns {
		requireWaitpointStatus(t, ctx, pool, orgID, waiting.runID, waiting.waitpointID, db.RunWaitStatusResuming)
		requireWaitpointConditionStatus(t, ctx, pool, orgID, waiting.waitpointID, db.WaitpointStatusExpired)
		requireRunStatus(t, ctx, pool, orgID, waiting.runID, db.RunStatusQueued)
		requireRunSnapshotTransitionCount(t, ctx, pool, orgID, waiting.runID, "waitpoint.timed_out", 1)
		requireRunEventKindCount(t, ctx, pool, orgID, waiting.runID, "waitpoint.resolved", 1)
	}
}

func TestConcurrentWaitpointTokenResponsesResolveOnce(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "token-concurrent-single")
	tokenID1 := seedWaitpointResponseToken(t, ctx, pool, orgID, runID, waitpointID, []byte{1}, "first@example.com")
	tokenID2 := seedWaitpointResponseToken(t, ctx, pool, orgID, runID, waitpointID, []byte{2}, "second@example.com")

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	q1 := db.New(tx1)
	if err := respondWaitpointToken(ctx, q1, orgID, waitpointID, tokenID1, []byte{1}, "token:first"); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		tx2, err := pool.Begin(ctx)
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		q2 := db.New(tx2)
		if err := respondWaitpointToken(ctx, q2, orgID, waitpointID, tokenID2, []byte{2}, "token:second"); err != nil {
			errCh <- err
			return
		}
		errCh <- tx2.Commit(ctx)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusResuming)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusQueued)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusQueued)
	requireWaitpointResponseCount(t, ctx, pool, orgID, runID, waitpointID, 1)
	requireRunEventKind(t, ctx, pool, orgID, runID, "waitpoint.resolved")
}

func TestMarkWaitpointDeliverySentWinsSameAttemptStaleRequeue(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "delivery-stale-sent")
	deliveryID := ids.ToPG(ids.New())
	future := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := queries.CreateQueuedWaitpointEmailDelivery(ctx, db.CreateQueuedWaitpointEmailDeliveryParams{
		DeliveryID:       deliveryID,
		OrgID:            orgID,
		RunID:            runID,
		WaitpointID:      waitpointID,
		TokenHash:        []byte{1},
		ExpiresAt:        pgTime(future),
		Recipient:        "owner@example.test",
		TokenMetadata:    []byte(`{}`),
		MessageID:        pgText("<waitpoint-delivery@example.test>"),
		DeliveryMetadata: []byte(`{"source":"test"}`),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimWaitpointDeliveryForSend(ctx, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.RequeueStaleSendingWaitpointDeliveries(ctx, db.RequeueStaleSendingWaitpointDeliveriesParams{
		StaleBefore: pgTime(future),
		MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	sent, err := queries.MarkWaitpointDeliverySent(ctx, db.MarkWaitpointDeliverySentParams{
		OrgID:            orgID,
		DeliveryID:       deliveryID,
		AttemptCount:     claimed.AttemptCount,
		SendingStartedAt: claimed.SendingStartedAt,
		LastAttemptAt:    claimed.LastAttemptAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != db.WaitpointDeliveryStatusSent {
		t.Fatalf("delivery status = %s, want sent", sent.Status)
	}
}

func TestReleaseRestoredExecutionFailureInvalidatesRestoreCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-restored-failure")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, "message-restored-failure")
	restoreCheckpointID := seedReadyRestoreCheckpoint(t, ctx, pool, orgID, runID, instance.ID)
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText("message-restored-failure"),
		DispatchLeaseID:   "lease-restored-failure",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecutionSession(ctx, db.ReleaseRunExecutionSessionParams{
		OrgID:                orgID,
		RunID:                runID,
		SessionID:            sessionID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    "message-restored-failure",
		DispatchLeaseID:      "lease-restored-failure",
		RunStatus:            db.RunStatusFailed,
		AttemptStatus:        db.RunAttemptStatusFailed,
		ErrorMessage:         pgText("restore failed"),
		TerminalEventKind:    "run.failed",
		TerminalEventPayload: []byte(`{"failure_kind":"worker_failed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireCheckpointStatus(t, ctx, pool, orgID, runID, restoreCheckpointID, db.CheckpointStatusInvalid)
}

func TestLostRunSessionsExhaustDispatchAttempts(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-lost-attempts")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)

	for attempt := int32(1); attempt <= 2; attempt++ {
		if _, err := pool.Exec(ctx, `
INSERT INTO run_execution_sessions (
    id,
    org_id,
    run_id,
    attempt_id,
    worker_instance_id,
    worker_group_id,
    dispatch_message_id,
    dispatch_lease_id,
    dispatch_attempt,
	    status,
	    lease_expires_at,
	    runtime_id,
	    trace_id,
	    span_id,
	    parent_span_id,
	    traceparent,
	    lost_at
		)
	SELECT $1,
	       $2,
	       $3,
	       runs.current_attempt_id,
	       $4,
	       $5,
	       $6,
	       $7,
	       $8,
	       'lost',
	       now() - interval '1 minute',
	       $9,
	       runs.trace_id,
	       '0123456789abcdef',
	       runs.root_span_id,
	       '00-' || runs.trace_id || '-0123456789abcdef-01',
	       now()
	  FROM runs
	 WHERE runs.org_id = $2
	   AND runs.id = $3
		`, ids.ToPG(ids.New()), orgID, runID, instance.ID, instance.WorkerGroupID, "message-lost", "lease-lost", attempt, instance.RuntimeID); err != nil {
			t.Fatal(err)
		}
	}
	exhausted, err := queries.RunExecutionSessionDispatchAttemptsExhausted(ctx, db.RunExecutionSessionDispatchAttemptsExhaustedParams{
		OrgID:               orgID,
		RunID:               runID,
		MaxDispatchAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !exhausted {
		t.Fatal("dispatch attempts were not exhausted")
	}
}

func TestDeadLetterRunQueueItemFailsQueuedRun(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "dead-letter-runner")
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "dead-letter-queue", instance, "dead-letter-message")

	deadLettered, err := queries.DeadLetterRunQueueItem(ctx, db.DeadLetterRunQueueItemParams{
		OrgID:             orgID,
		RunID:             runID,
		DispatchMessageID: pgText("dead-letter-message"),
		LastError:         "delivery exhausted",
		EventKind:         "run.dead_lettered",
		EventPayload:      []byte(`{"reason":"max_dispatch_attempts_exceeded"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if deadLettered.Status != db.RunQueueStatusDeadLettered {
		t.Fatalf("dead letter status = %s", deadLettered.Status)
	}
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusDeadLettered)
	requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusFailed)
	requireCurrentRunAttemptStatus(t, ctx, pool, orgID, runID, db.RunAttemptStatusFailed)
	requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "run.dead_lettered", 1)
	requireRunEventKindCount(t, ctx, pool, orgID, runID, "run.dead_lettered", 1)
}

func seedReadyRestoreCheckpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, workerInstanceID pgtype.UUID) pgtype.UUID {
	t.Helper()
	sessionID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
	INSERT INTO run_execution_sessions (
	    id,
	    org_id,
	    run_id,
		    attempt_id,
	    worker_instance_id,
	    worker_group_id,
	    dispatch_message_id,
	    dispatch_lease_id,
	    dispatch_attempt,
	    status,
	    lease_expires_at,
	    runtime_id,
	    active_duration_ms,
	    trace_id,
	    span_id,
	    parent_span_id,
	    traceparent,
	    released_at
		)
	SELECT $1,
	       $2,
	       $3,
	       runs.current_attempt_id,
	       $4,
	       (SELECT worker_group_id FROM worker_instances WHERE id = $4),
	       'previous-message',
	       'previous-lease',
	       1,
	       'detached',
	       now() + interval '1 minute',
	       'sha256:runtime',
	       100,
	       runs.trace_id,
	       'fedcba9876543210',
	       runs.root_span_id,
	       '00-' || runs.trace_id || '-fedcba9876543210-01',
	       now()
	  FROM runs
	 WHERE runs.org_id = $2
	   AND runs.id = $3
	`, sessionID, orgID, runID, workerInstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoints (
	    id,
	    org_id,
	    run_id,
	    project_id,
	    environment_id,
	    session_id,
	    status,
	    reason,
	    manifest,
	    ready_at
	)
	SELECT $1::uuid,
	       runs.org_id,
	       runs.id,
	       runs.project_id,
	       runs.environment_id,
	       $4,
	       'ready',
	       'waitpoint',
	       '{"runtime":{"backend":"firecracker"}}',
	       now()
	  FROM runs
	 WHERE runs.org_id = $2
	   AND runs.id = $3
	`, checkpointID, orgID, runID, sessionID); err != nil {
		t.Fatal(err)
	}
	runtimeConfigArtifactID := ids.ToPG(ids.New())
	workspaceArtifactID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
	INSERT INTO cas_objects (digest, size_bytes, media_type)
	VALUES
	    ('sha256:runtime-config', 1, 'application/vnd.helmr.checkpoint.runtime-config.v0+json'),
	    ($1, 1, 'application/vnd.helmr.workspace.v0.tar')
	ON CONFLICT (digest) DO NOTHING
	`, testDigest("6")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
	SELECT $1,
	       runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       'sha256:runtime-config',
	       'checkpoint_runtime_config'::artifact_kind,
	       1,
	       'application/vnd.helmr.checkpoint.runtime-config.v0+json'
	  FROM runs
	 WHERE runs.org_id = $3
	   AND runs.id = $4
	UNION ALL
	SELECT $2::uuid,
	       runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       $5,
	       'checkpoint_workspace'::artifact_kind,
	       1,
	       'application/vnd.helmr.workspace.v0.tar'
	  FROM runs
	 WHERE runs.org_id = $3
	   AND runs.id = $4
	`, runtimeConfigArtifactID, workspaceArtifactID, orgID, runID, testDigest("6")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoint_runtime_snapshots (
	    org_id,
	    project_id,
	    environment_id,
	    run_id,
	    checkpoint_id,
	    runtime_backend,
	    runtime_id,
	    runtime_arch,
	    runtime_abi,
	    kernel_digest,
	    initramfs_digest,
	    rootfs_digest,
	    cni_profile,
	    runtime_config_artifact_id
	)
	SELECT runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       runs.id,
	       $3,
	       'firecracker',
	       'sha256:runtime',
	       'x86_64',
	       'helmr.firecracker.snapshot.v0',
	       'sha256:kernel',
	       'sha256:initramfs',
	       'sha256:rootfs',
	       'helmr/v0',
	       $4
	  FROM runs
	 WHERE runs.org_id = $1
	   AND runs.id = $2
	`, orgID, runID, checkpointID, runtimeConfigArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	INSERT INTO checkpoint_workspace_snapshots (
	    org_id,
	    project_id,
	    environment_id,
	    run_id,
	    checkpoint_id,
	    workspace_artifact_id,
		    workspace_artifact_encoding,
		    workspace_mount_path,
		    workspace_volume_kind
		)
	SELECT runs.org_id,
	       runs.project_id,
	       runs.environment_id,
	       runs.id,
	       $3,
	       $4,
	       'tar',
	       '/workspace',
	       'copy-on-write'
	  FROM runs
	 WHERE runs.org_id = $1
	   AND runs.id = $2
		`, orgID, runID, checkpointID, workspaceArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	WITH run_scope AS (
	    SELECT org_id, id AS run_id, project_id, environment_id
	      FROM runs
	     WHERE org_id = $2
	       AND id = $3
	),
	waitpoint AS (
	    INSERT INTO waitpoints (
	        id,
	        org_id,
	        project_id,
	        environment_id,
	        kind,
	        request,
	        display_text,
	        status,
	        resolution_kind,
	        output,
	        completed_at
	    )
	    SELECT $1,
	           run_scope.org_id,
	           run_scope.project_id,
	           run_scope.environment_id,
	           'human',
	           '{}',
	           'approve',
	           'completed',
	           'completed',
	           '{"value":{"approved":true}}',
	           now()
	      FROM run_scope
	    RETURNING *
	),
	run_wait AS (
	    INSERT INTO run_waits (
	        id,
	        org_id,
	        run_id,
	        project_id,
	        environment_id,
	        session_id,
	        checkpoint_id,
	        correlation_id,
	        status,
	        resolution_kind,
	        resolution,
	        waiting_at,
	        resolved_at
	    )
	    SELECT $6,
	           run_scope.org_id,
	           run_scope.run_id,
	           run_scope.project_id,
	           run_scope.environment_id,
	           $4,
	           $5,
	           'restore-waitpoint',
	           'resuming',
	           'completed',
	           '{"value":{"approved":true}}',
	           now(),
	           now()
	      FROM waitpoint
	      JOIN run_scope ON true
	    RETURNING *
	)
	INSERT INTO run_wait_dependencies (
	    org_id,
	    run_id,
	    project_id,
	    environment_id,
	    run_wait_id,
	    waitpoint_id
	)
	SELECT run_wait.org_id,
	       run_wait.run_id,
	       run_wait.project_id,
	       run_wait.environment_id,
	       run_wait.id,
	       waitpoint.id
	  FROM run_wait
	  JOIN waitpoint ON true
	`, waitpointID, orgID, runID, sessionID, checkpointID, runWaitID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
	UPDATE runs
	   SET latest_checkpoint_id = $1
	 WHERE org_id = $2
	   AND id = $3
	`, checkpointID, orgID, runID); err != nil {
		t.Fatal(err)
	}
	return checkpointID
}

func requireCheckpointStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID, want db.CheckpointStatus) {
	t.Helper()
	var got db.CheckpointStatus
	if err := pool.QueryRow(ctx, `
	SELECT status
	  FROM checkpoints
	 WHERE org_id = $1
	   AND run_id = $2
	   AND id = $3
	`, orgID, runID, checkpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("checkpoint status = %s, want %s", got, want)
	}
}

func requireRuntimeConfigArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID) {
	t.Helper()
	var artifactID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT runtime_config_artifact_id
  FROM checkpoint_runtime_snapshots
 WHERE org_id = $1
   AND run_id = $2
   AND checkpoint_id = $3
`, orgID, runID, checkpointID).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	if !artifactID.Valid {
		t.Fatal("runtime_config_artifact_id is null")
	}
}

func requireNoCheckpointArtifacts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
  FROM checkpoint_artifacts
 WHERE org_id = $1
   AND run_id = $2
   AND checkpoint_id = $3
`, orgID, runID, checkpointID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("checkpoint artifact rows = %d, want 0", count)
	}
}

func requireWaitpointForCheckpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, checkpointID pgtype.UUID) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	var runWaitID pgtype.UUID
	var waitpointID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT run_waits.id, run_wait_dependencies.waitpoint_id
  FROM run_waits
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = run_waits.id
 WHERE run_waits.org_id = $1
   AND run_waits.run_id = $2
   AND run_waits.checkpoint_id = $3
	`, orgID, runID, checkpointID).Scan(&runWaitID, &waitpointID); err != nil {
		t.Fatal(err)
	}
	return runWaitID, waitpointID
}

func requireWaitpointStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, want db.RunWaitStatus) {
	t.Helper()
	var got db.RunWaitStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM run_waits
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = run_waits.org_id
                            AND run_wait_dependencies.run_wait_id = run_waits.id
 WHERE run_waits.org_id = $1
   AND run_waits.run_id = $2
   AND run_wait_dependencies.waitpoint_id = $3
`, orgID, runID, waitpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("waitpoint status = %s, want %s", got, want)
	}
}

func requireWaitpointConditionStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, waitpointID pgtype.UUID, want db.WaitpointStatus) {
	t.Helper()
	var got db.WaitpointStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM waitpoints
 WHERE org_id = $1
   AND id = $2
`, orgID, waitpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("waitpoint condition status = %s, want %s", got, want)
	}
}

func requireRunStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, want db.RunStatus) {
	t.Helper()
	var got db.RunStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run status = %s, want %s", got, want)
	}
}

func requireRunStateVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID) int64 {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, `
SELECT state_version
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func requireCurrentRunAttemptStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, want db.RunAttemptStatus) {
	t.Helper()
	var got db.RunAttemptStatus
	if err := pool.QueryRow(ctx, `
SELECT run_attempts.status
  FROM runs
  JOIN run_attempts ON run_attempts.org_id = runs.org_id
                   AND run_attempts.run_id = runs.id
                   AND run_attempts.id = runs.current_attempt_id
 WHERE runs.org_id = $1
   AND runs.id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("current attempt status = %s, want %s", got, want)
	}
}

func requireRunRetryDecision(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID, wantDecision db.RunRetryDecisionKind, wantReason string, wantNextAttempt int32) {
	t.Helper()
	var decision db.RunRetryDecisionKind
	var reason string
	var retryAfter pgtype.Timestamptz
	var nextAttempt pgtype.Int4
	if err := pool.QueryRow(ctx, `
SELECT decision,
       reason,
       retry_after,
       next_attempt_number
  FROM run_retry_decisions
 WHERE org_id = $1
   AND run_id = $2
   AND session_id = $3
`, orgID, runID, sessionID).Scan(&decision, &reason, &retryAfter, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if decision != wantDecision || reason != wantReason {
		t.Fatalf("retry decision = %s/%q, want %s/%q", decision, reason, wantDecision, wantReason)
	}
	if !retryAfter.Valid || !retryAfter.Time.After(time.Now()) {
		t.Fatalf("retry_after = %+v, want future timestamp", retryAfter)
	}
	if !nextAttempt.Valid || nextAttempt.Int32 != wantNextAttempt {
		t.Fatalf("next_attempt_number = %+v, want %d", nextAttempt, wantNextAttempt)
	}
}

func requireRunUsageEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string, wantCount int, wantQuantity int64) {
	t.Helper()
	var gotCount int
	var gotQuantity int64
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int,
       COALESCE(sum(quantity), 0)::bigint
  FROM run_usage_events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&gotCount, &gotQuantity); err != nil {
		t.Fatal(err)
	}
	if gotCount != wantCount || gotQuantity != wantQuantity {
		t.Fatalf("usage %s count/quantity = %d/%d, want %d/%d", kind, gotCount, gotQuantity, wantCount, wantQuantity)
	}
}

func requireRunUsageEventPositive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string, wantCount int) {
	t.Helper()
	var gotCount int
	var gotQuantity int64
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int,
       COALESCE(sum(quantity), 0)::bigint
  FROM run_usage_events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&gotCount, &gotQuantity); err != nil {
		t.Fatal(err)
	}
	if gotCount != wantCount || gotQuantity <= 0 {
		t.Fatalf("usage %s count/quantity = %d/%d, want %d/positive", kind, gotCount, gotQuantity, wantCount)
	}
}

func requireRunUsageEventAtLeast(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string, wantCount int, minQuantity int64) {
	t.Helper()
	var gotCount int
	var gotQuantity int64
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int,
       COALESCE(sum(quantity), 0)::bigint
  FROM run_usage_events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&gotCount, &gotQuantity); err != nil {
		t.Fatal(err)
	}
	if gotCount != wantCount || gotQuantity < minQuantity {
		t.Fatalf("usage %s count/quantity = %d/%d, want %d/>=%d", kind, gotCount, gotQuantity, wantCount, minQuantity)
	}
}

func requireRunUsageDuration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, `
SELECT usage_duration_ms
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run usage_duration_ms = %d, want %d", got, want)
	}
}

func requireRunUsageEventSnapshotTransition(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind, wantTransition string) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_usage_events
  JOIN run_snapshots
    ON run_snapshots.org_id = run_usage_events.org_id
   AND run_snapshots.run_id = run_usage_events.run_id
   AND run_snapshots.version = run_usage_events.snapshot_version
 WHERE run_usage_events.org_id = $1
   AND run_usage_events.run_id = $2
   AND run_usage_events.kind = $3
   AND run_snapshots.transition = $4
`, orgID, runID, kind, wantTransition).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == 0 {
		t.Fatalf("usage %s has no snapshot transition %q", kind, wantTransition)
	}
}

func requireRunExecutionSessionActiveDuration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, `
SELECT active_duration_ms
  FROM run_execution_sessions
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("session active_duration_ms = %d, want %d", got, want)
	}
}

func requireRunExecutionSessionStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID, want db.RunExecutionSessionStatus) {
	t.Helper()
	var got db.RunExecutionSessionStatus
	var lostAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
SELECT status, lost_at
  FROM run_execution_sessions
 WHERE org_id = $1
   AND run_id = $2
   AND id = $3
`, orgID, runID, sessionID).Scan(&got, &lostAt); err != nil {
		t.Fatal(err)
	}
	if got != want || (want == db.RunExecutionSessionStatusLost && !lostAt.Valid) {
		t.Fatalf("run execution status = %s lost_at = %+v, want %s", got, lostAt, want)
	}
}

func requireNoActiveConcurrencySlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_queue_concurrency_leases
 WHERE org_id = $1
   AND run_id = $2
   AND session_id = $3
   AND released_at IS NULL
`, orgID, runID, sessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("active concurrency slots = %d, want 0", count)
	}
}

func requireActiveConcurrencySlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_queue_concurrency_leases
 WHERE org_id = $1
   AND run_id = $2
   AND session_id = $3
   AND released_at IS NULL
`, orgID, runID, sessionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("active concurrency slots = %d, want 1", count)
	}
}

func requireWaitpointResponseCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM waitpoint_responses
 WHERE org_id = $1
   AND waitpoint_id = $2
`, orgID, waitpointID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("waitpoint response count = %d, want %d", got, want)
	}
}

func requireWaitpointCompletionPayloads(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, wantOutput, wantResolution []byte) {
	t.Helper()
	var output, waitpointResolution, runWaitResolution, responseResolution []byte
	if err := pool.QueryRow(ctx, `
SELECT waitpoints.output,
       waitpoints.resolution,
       run_waits.resolution,
       waitpoint_responses.resolution
  FROM waitpoints
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waitpoints.org_id
                            AND run_wait_dependencies.waitpoint_id = waitpoints.id
  JOIN run_waits ON run_waits.org_id = run_wait_dependencies.org_id
                AND run_waits.run_id = $2
                AND run_waits.id = run_wait_dependencies.run_wait_id
  JOIN waitpoint_responses ON waitpoint_responses.org_id = waitpoints.org_id
                          AND waitpoint_responses.waitpoint_id = waitpoints.id
 WHERE waitpoints.org_id = $1
   AND waitpoints.id = $3
`, orgID, runID, waitpointID).Scan(&output, &waitpointResolution, &runWaitResolution, &responseResolution); err != nil {
		t.Fatal(err)
	}
	requireCanonicalJSON(t, "waitpoint output", output, wantOutput)
	requireCanonicalJSON(t, "waitpoint resolution", waitpointResolution, wantResolution)
	requireCanonicalJSON(t, "run wait resolution", runWaitResolution, wantResolution)
	requireCanonicalJSON(t, "waitpoint response resolution", responseResolution, wantResolution)
}

func requireCancelledWaitpointPayloads(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, wantResolution []byte) {
	t.Helper()
	var output, waitpointResolution, runWaitResolution, runWaitFailure []byte
	var outputIsError bool
	if err := pool.QueryRow(ctx, `
SELECT waitpoints.output,
       waitpoints.resolution,
       waitpoints.output_is_error,
       run_waits.resolution,
       run_waits.failure
  FROM waitpoints
  JOIN run_wait_dependencies ON run_wait_dependencies.org_id = waitpoints.org_id
                            AND run_wait_dependencies.waitpoint_id = waitpoints.id
  JOIN run_waits ON run_waits.org_id = run_wait_dependencies.org_id
                AND run_waits.run_id = $2
                AND run_waits.id = run_wait_dependencies.run_wait_id
 WHERE waitpoints.org_id = $1
   AND waitpoints.id = $3
`, orgID, runID, waitpointID).Scan(&output, &waitpointResolution, &outputIsError, &runWaitResolution, &runWaitFailure); err != nil {
		t.Fatal(err)
	}
	if !outputIsError {
		t.Fatal("waitpoint output_is_error = false, want true")
	}
	requireCanonicalJSON(t, "cancelled waitpoint output", output, []byte(`null`))
	requireCanonicalJSON(t, "cancelled waitpoint resolution", waitpointResolution, wantResolution)
	requireCanonicalJSON(t, "cancelled run wait resolution", runWaitResolution, wantResolution)
	requireCanonicalJSON(t, "cancelled run wait failure", runWaitFailure, wantResolution)
}

func requireCanonicalJSON(t *testing.T, name string, got []byte, want []byte) {
	t.Helper()
	gotCanonical := canonicalJSON(t, got)
	wantCanonical := canonicalJSON(t, want)
	if gotCanonical != wantCanonical {
		t.Fatalf("%s = %s, want %s", name, gotCanonical, wantCanonical)
	}
}

func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("invalid JSON %q: %v", string(raw), err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(canonical)
}

func requireRunEventKind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("run event %q not found", kind)
	}
}

func requireRunEventKindCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run event %q count = %d, want %d", kind, got, want)
	}
}

func requireRunEventObservability(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, projectID, environmentID, runID pgtype.UUID, kind, wantCategory, wantSeverity, wantSource, wantRedactionClass string, wantSession bool) {
	t.Helper()
	var gotProjectID pgtype.UUID
	var gotEnvironmentID pgtype.UUID
	var gotAttemptID pgtype.UUID
	var gotSessionID pgtype.UUID
	var traceID string
	var spanID pgtype.Text
	var parentSpanID pgtype.Text
	var traceparent pgtype.Text
	var category string
	var severity string
	var source string
	var message string
	var redactionClass string
	var snapshotVersion pgtype.Int8
	if err := pool.QueryRow(ctx, `
SELECT project_id,
       environment_id,
       attempt_id,
       session_id,
       trace_id,
       span_id,
       parent_span_id,
       traceparent,
       category,
       severity,
       source,
       message,
       redaction_class,
       snapshot_version
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
 ORDER BY id DESC
 LIMIT 1
`, orgID, runID, kind).Scan(
		&gotProjectID,
		&gotEnvironmentID,
		&gotAttemptID,
		&gotSessionID,
		&traceID,
		&spanID,
		&parentSpanID,
		&traceparent,
		&category,
		&severity,
		&source,
		&message,
		&redactionClass,
		&snapshotVersion,
	); err != nil {
		t.Fatal(err)
	}
	if gotProjectID != projectID || gotEnvironmentID != environmentID {
		t.Fatalf("event %q scope = %v/%v, want %v/%v", kind, gotProjectID, gotEnvironmentID, projectID, environmentID)
	}
	if !gotAttemptID.Valid {
		t.Fatalf("event %q attempt_id is null", kind)
	}
	if len(traceID) != 32 {
		t.Fatalf("event %q trace_id = %q", kind, traceID)
	}
	if category != wantCategory || severity != wantSeverity || source != wantSource || redactionClass != wantRedactionClass || message != kind {
		t.Fatalf("event %q envelope category/severity/source/redaction/message = %q/%q/%q/%q/%q, want %q/%q/%q/%q/%q", kind, category, severity, source, redactionClass, message, wantCategory, wantSeverity, wantSource, wantRedactionClass, kind)
	}
	if !snapshotVersion.Valid || snapshotVersion.Int64 <= 0 {
		t.Fatalf("event %q snapshot_version = %+v", kind, snapshotVersion)
	}
	if wantSession {
		if !gotSessionID.Valid || !spanID.Valid || len(spanID.String) != 16 || !parentSpanID.Valid || len(parentSpanID.String) != 16 || !traceparent.Valid || !strings.Contains(traceparent.String, traceID+"-"+spanID.String) {
			t.Fatalf("event %q session trace fields session=%+v span=%+v parent=%+v traceparent=%+v", kind, gotSessionID, spanID, parentSpanID, traceparent)
		}
	} else if gotSessionID.Valid {
		t.Fatalf("event %q session_id = %+v, want null", kind, gotSessionID)
	}
}

func requireRunSnapshotTransitionCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, transition string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM run_snapshots
 WHERE org_id = $1
   AND run_id = $2
   AND transition = $3
`, orgID, runID, transition).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run snapshot transition %q count = %d, want %d", transition, got, want)
	}
}

func requireRunSessionEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, sessionID pgtype.UUID, attemptNumber int32, kind string, wantPayload []byte) {
	t.Helper()
	var gotSessionID pgtype.UUID
	var gotAttemptNumber pgtype.Int4
	var gotPayload []byte
	if err := pool.QueryRow(ctx, `
SELECT session_id, attempt_number, payload
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND session_id = $3
   AND kind = $4
 ORDER BY id DESC
 LIMIT 1
`, orgID, runID, sessionID, kind).Scan(&gotSessionID, &gotAttemptNumber, &gotPayload); err != nil {
		t.Fatal(err)
	}
	if gotSessionID != sessionID || !gotAttemptNumber.Valid || gotAttemptNumber.Int32 != attemptNumber {
		t.Fatalf("run event %q session = %+v attempt = %+v, want session %s attempt %d", kind, gotSessionID, gotAttemptNumber, ids.MustFromPG(sessionID), attemptNumber)
	}
	requireCanonicalJSON(t, "run event payload", gotPayload, wantPayload)
}

func requireNoRunEventKind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID pgtype.UUID, kind string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int
  FROM events
 WHERE org_id = $1
   AND run_id = $2
   AND kind = $3
`, orgID, runID, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("run event %q count = %d, want 0", kind, count)
	}
}

func seedWaitingWaitpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, orgID pgtype.UUID, suffix string) (pgtype.UUID, pgtype.UUID) {
	t.Helper()
	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	instance := upsertTestWorkerInstance(t, ctx, queries, "runner-"+suffix)
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	messageID := "message-" + suffix
	seedLeasableRunQueueItem(t, ctx, queries, orgID, runID, "exec-queue", instance, messageID)
	sessionID := ids.ToPG(ids.New())
	if _, err := queries.LeaseRunExecutionSession(ctx, db.LeaseRunExecutionSessionParams{
		OrgID:             orgID,
		RunID:             runID,
		WorkerInstanceID:  instance.ID,
		SessionID:         sessionID,
		DispatchMessageID: pgText(messageID),
		DispatchLeaseID:   "lease-" + suffix,
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgTime(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecutionSession(ctx, db.StartRunExecutionSessionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
	}); err != nil {
		t.Fatal(err)
	}
	checkpointID := ids.ToPG(ids.New())
	runWaitID := ids.ToPG(ids.New())
	waitpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		CorrelationID:    suffix,
		CheckpointID:     checkpointID,
		CheckpointReason: "waitpoint",
		RunWaitID:        runWaitID,
		ID:               waitpointID,
		Kind:             db.WaitpointKindHuman,
		Request:          []byte(`{"message":"approve"}`),
		DisplayText:      "approve",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointCheckpointDurableReady(ctx, db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      orgID,
		RunID:                      runID,
		SessionID:                  sessionID,
		WorkerInstanceID:           instance.ID,
		RunWaitID:                  runWaitID,
		WaitpointID:                waitpointID,
		CheckpointID:               checkpointID,
		CheckpointArtifacts:        testCheckpointArtifactsJSON(t),
		Manifest:                   []byte(`{"runtime":{"backend":"firecracker"}}`),
		RuntimeBackend:             "firecracker",
		RuntimeID:                  instance.RuntimeID,
		RuntimeArch:                "x86_64",
		RuntimeABI:                 "helmr.firecracker.snapshot.v0",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "helmr/v0",
		WorkspaceArtifactDigest:    pgText(testDigest("5")),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 1, Valid: true},
		WorkspaceArtifactMediaType: pgText("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:  pgText("tar"),
		WorkspaceMountPath:         pgText("/workspace"),
		WorkspaceVolumeKind:        pgText("copy-on-write"),
		ActiveDurationMs:           100,
		CheckpointPayload:          []byte(`{"checkpoint_id":"next"}`),
	}); err != nil {
		t.Fatal(err)
	}
	requireWaitpointStatus(t, ctx, pool, orgID, runID, waitpointID, db.RunWaitStatusWaiting)
	requireRunQueueItemStatus(t, ctx, pool, orgID, runID, db.RunQueueStatusSuspended)
	return runID, waitpointID
}

func seedWaitpointResponseToken(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, runID, waitpointID pgtype.UUID, tokenHash []byte, externalSubject string) pgtype.UUID {
	t.Helper()
	tokenID := ids.ToPG(ids.New())
	if _, err := pool.Exec(ctx, `
INSERT INTO waitpoint_response_tokens (id, org_id, project_id, environment_id, waitpoint_id, token_hash, expires_at, external_subject, metadata)
SELECT $1, $2, waitpoints.project_id, waitpoints.environment_id, $4, $5, now() + interval '5 minutes', $6, '{}'
  FROM run_wait_dependencies
  JOIN waitpoints ON waitpoints.org_id = run_wait_dependencies.org_id
                 AND waitpoints.id = run_wait_dependencies.waitpoint_id
 WHERE run_wait_dependencies.org_id = $2
   AND run_wait_dependencies.run_id = $3
   AND run_wait_dependencies.waitpoint_id = $4
`, tokenID, orgID, runID, waitpointID, tokenHash, externalSubject); err != nil {
		t.Fatal(err)
	}
	return tokenID
}

func respondWaitpointToken(ctx context.Context, queries *db.Queries, orgID, waitpointID, tokenID pgtype.UUID, tokenHash []byte, responseKey string) error {
	if _, err := queries.MarkWaitpointResponseTokenCompleted(ctx, db.MarkWaitpointResponseTokenCompletedParams{
		OrgID:                orgID,
		ID:                   tokenID,
		TokenHash:            tokenHash,
		CompletedByPrincipal: pgText(responseKey),
		CompletedVia:         pgText("email_token"),
		Metadata:             []byte(`{}`),
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		OrgID:                orgID,
		WaitpointID:          waitpointID,
		ResponseKey:          responseKey,
		RequestHash:          responseKey,
		Action:               "respond",
		Kind:                 db.WaitpointKindHuman,
		ResolutionKind:       pgText("completed"),
		Resolution:           approvedWaitpointResolution(responseKey),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgText(responseKey),
		CompletedVia:         pgText("email_token"),
		Metadata:             []byte(`{}`),
	}); err != nil {
		return err
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveApprovedWaitpointParams(orgID, pgtype.UUID{}, waitpointID)); err != nil {
		return err
	}
	_, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{OrgID: orgID, WaitpointID: waitpointID})
	return err
}

func resolveApprovedWaitpointParams(orgID, runID, waitpointID pgtype.UUID) db.ResolveWaitpointParams {
	return db.ResolveWaitpointParams{
		OrgID:          orgID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindHuman,
		ResolutionKind: pgText("completed"),
		Output:         []byte(`{"approved":true}`),
		Resolution:     approvedWaitpointResolution("reviewer@example.com"),
	}
}

func approvedWaitpointResolution(principal string) []byte {
	payload, err := json.Marshal(map[string]any{
		"value":     map[string]any{"approved": true},
		"principal": principal,
		"at":        "2026-04-23T00:00:00Z",
	})
	if err != nil {
		panic(err)
	}
	return payload
}

func testCheckpointArtifactsJSON(t *testing.T) []byte {
	t.Helper()
	rows := []map[string]any{
		{"role": "runtime_config", "ordinal": 0, "digest": testDigest("1"), "size_bytes": 1, "media_type": cas.CheckpointRuntimeConfigMediaType},
		{"role": "runtime_vmstate", "ordinal": 0, "digest": testDigest("2"), "size_bytes": 2, "media_type": cas.CheckpointVMStateMediaType},
		{"role": "runtime_memory", "ordinal": 0, "digest": testDigest("3"), "size_bytes": 3, "media_type": cas.CheckpointMemoryMediaType},
		{"role": "runtime_scratch_disk", "ordinal": 0, "digest": testDigest("4"), "size_bytes": 4, "media_type": cas.CheckpointScratchDiskMediaType},
	}
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func testDigest(char string) string {
	return "sha256:" + strings.Repeat(char, 64)
}

func seedLeasableRunQueueItem(t *testing.T, ctx context.Context, queries *db.Queries, orgID, runID pgtype.UUID, queueName string, instance db.UpsertWorkerInstanceHeartbeatRow, messageID string) {
	t.Helper()
	if _, err := queries.UpsertRunRuntimeRequirements(ctx, db.UpsertRunRuntimeRequirementsParams{
		RunID:                   runID,
		OrgID:                   orgID,
		RequestedMilliCpu:       1000,
		RequestedMemoryMib:      1024,
		RequestedDiskMib:        2048,
		RequestedExecutionSlots: 1,
		RuntimeID:               instance.RuntimeID,
		RuntimeArch:             "x86_64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CniProfile:              "helmr/v0",
		NetworkPolicy:           []byte(`{}`),
		Placement:               []byte(`{}`),
		WorkerGroupID:           instance.WorkerGroupID,
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := queries.UpsertRunQueueItemQueued(ctx, db.UpsertRunQueueItemQueuedParams{
		RunID:             runID,
		OrgID:             orgID,
		Priority:          10,
		QueueName:         queueName,
		QueueTimestamp:    pgTime(time.Now()),
		DispatchMessageID: pgText(messageID),
	})
	if err != nil {
		t.Fatal(err)
	}
	publishTestRunQueueItem(t, ctx, queries, orgID, runID, entry, messageID)
	if _, err := queries.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    pgText(messageID),
		ReservationExpiresAt: pgTime(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
}

type runQueueItemDispatchState struct {
	Status                     db.RunQueueStatus
	QueuedExpiresAt            pgtype.Timestamptz
	DispatchMessageID          pgtype.Text
	ReservedByWorkerInstanceID pgtype.UUID
	ReservationExpiresAt       pgtype.Timestamptz
	DispatchGeneration         int64
	LastError                  string
}

func requireRunQueueItemDispatchState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, runID pgtype.UUID) runQueueItemDispatchState {
	t.Helper()
	var got runQueueItemDispatchState
	if err := pool.QueryRow(ctx, `
SELECT status,
       queued_expires_at,
       dispatch_message_id,
       reserved_by_worker_instance_id,
       reservation_expires_at,
       dispatch_generation,
       last_error
  FROM run_queue_items
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID).Scan(
		&got.Status,
		&got.QueuedExpiresAt,
		&got.DispatchMessageID,
		&got.ReservedByWorkerInstanceID,
		&got.ReservationExpiresAt,
		&got.DispatchGeneration,
		&got.LastError,
	); err != nil {
		t.Fatal(err)
	}
	return got
}

func requireRunQueueItemStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, runID pgtype.UUID, want db.RunQueueStatus) {
	t.Helper()
	var got db.RunQueueStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM run_queue_items
 WHERE org_id = $1
   AND run_id = $2
`, orgID, runID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("run queue status = %s, want %s", got, want)
	}
}
