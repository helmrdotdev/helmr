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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestLeaseRunExecutionSessionSeparatesWorkerGroupsWithinSharedQueue(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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

func TestCancelRequeuedLeasedRunFinalizesQueueItem(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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

func TestLostRunSessionsExhaustDispatchAttempts(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
