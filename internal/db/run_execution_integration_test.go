package db_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestLeaseRunExecutionSessionBindsWorkerInstanceDispatchLease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-a"),
		DispatchLeaseID:   "lease-a",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
		DispatchMessageID: pgvalue.Text("message-a"),
		DispatchLeaseID:   "lease-b",
		DispatchAttempt:   2,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
	stdoutChunk, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		Stream:           db.RunLogStreamStdout,
		ObservedSeq:      1,
		Content:          []byte("hello"),
		Kind:             "log",
		Payload:          []byte(`{"stream":"stdout"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	stderrChunk, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		OrgID:            orgID,
		RunID:            runID,
		SessionID:        sessionID,
		WorkerInstanceID: instance.ID,
		Stream:           db.RunLogStreamStderr,
		ObservedSeq:      1,
		Content:          []byte("warn"),
		Kind:             "log",
		Payload:          []byte(`{"stream":"stderr"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdoutChunk.Seq != 1 || stderrChunk.Seq != 2 {
		t.Fatalf("log chunk seqs = stdout %d stderr %d, want 1,2", stdoutChunk.Seq, stderrChunk.Seq)
	}
	logChunks, err := queries.ListRunLogChunksAfter(ctx, db.ListRunLogChunksAfterParams{
		OrgID:    orgID,
		RunID:    runID,
		Seq:      0,
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(logChunks) != 2 || logChunks[0].Seq != 1 || logChunks[0].Stream != db.RunLogStreamStdout || logChunks[1].Seq != 2 || logChunks[1].Stream != db.RunLogStreamStderr {
		t.Fatalf("log chunks = %+v", logChunks)
	}
	logSnapshot, err := queries.GetRunLogSnapshot(ctx, db.GetRunLogSnapshotParams{
		OrgID:       orgID,
		RunID:       runID,
		StdoutLimit: 1024,
		StderrLimit: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(logSnapshot.Stdout) != "hello" || string(logSnapshot.Stderr) != "warn" || logSnapshot.Cursor != 2 || logSnapshot.StdoutBytes != int64(len("hello")) || logSnapshot.StderrBytes != int64(len("warn")) || logSnapshot.Truncated.Bool {
		t.Fatalf("log snapshot = %+v", logSnapshot)
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
	requireRunUsageEvent(t, ctx, pool, orgID, runID, "log_bytes", 2, int64(len("hello")+len("warn")))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, input := range []struct {
		stream db.RunLogStream
		seq    int64
		body   string
	}{
		{stream: db.RunLogStreamStdout, seq: 2, body: "more"},
		{stream: db.RunLogStreamStderr, seq: 2, body: "noise"},
	} {
		wg.Add(1)
		go func(input struct {
			stream db.RunLogStream
			seq    int64
			body   string
		}) {
			defer wg.Done()
			_, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
				OrgID:            orgID,
				RunID:            runID,
				SessionID:        sessionID,
				WorkerInstanceID: instance.ID,
				Stream:           input.stream,
				ObservedSeq:      input.seq,
				Content:          []byte(input.body),
				Kind:             "log",
				Payload:          []byte(`{"stream":"concurrent"}`),
			})
			errs <- err
		}(input)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	concurrentChunks, err := queries.ListRunLogChunksAfter(ctx, db.ListRunLogChunksAfterParams{
		OrgID:    orgID,
		RunID:    runID,
		Seq:      2,
		RowLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(concurrentChunks) != 2 || concurrentChunks[0].Seq != 3 || concurrentChunks[1].Seq != 4 {
		t.Fatalf("concurrent log chunks = %+v", concurrentChunks)
	}
	if _, err := queries.RenewRunQueueReservation(ctx, db.RenewRunQueueReservationParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     instance.ID,
		DispatchMessageID:    pgvalue.Text("message-a"),
		ReservationExpiresAt: pgvalue.Timestamptz(time.Now().Add(2 * time.Minute)),
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
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(2 * time.Minute)),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-retry"),
		DispatchLeaseID:   "lease-retry",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-graceful-cancel"),
		DispatchLeaseID:   "lease-graceful-cancel",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(2 * time.Minute)),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-cancel-leased"),
		DispatchLeaseID:   "lease-cancel-leased",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-force-cancel"),
		DispatchLeaseID:   "lease-force-cancel",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-worker-group"),
		DispatchLeaseID:   "lease-worker-group",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		SessionSpanID:     "0123456789abcdef",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lease error = %v, want no rows", err)
	}
}

func TestFailExpiredRunningRunExecutionSessionsSchedulesInfraLostRetry(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-expired-retry"),
		DispatchLeaseID:   "lease-expired-retry",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
			DispatchMessageID: pgvalue.Text(messageID),
			DispatchLeaseID:   "lease-expired-running-multi-" + suffix,
			DispatchAttempt:   1,
			LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-graceful-cancel-expired"),
		DispatchLeaseID:   "lease-graceful-cancel-expired",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-force-cancel-pending"),
		DispatchLeaseID:   "lease-force-cancel-pending",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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
