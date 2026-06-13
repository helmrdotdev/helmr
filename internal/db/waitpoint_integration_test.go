package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestFailExpiredRunningRunExecutionSessionsSweepsOpeningWaitpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-opening"),
		DispatchLeaseID:   "lease-opening",
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

func TestReleaseRunExecutionSessionSeparatesCancelledWaitpointOutputAndResolution(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text(messageID),
		DispatchLeaseID:   "lease-release-cancelled-waitpoint",
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
		ErrorMessage:         pgvalue.Text("worker failed"),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
		DispatchMessageID: pgvalue.Text("message-leased-waitpoint"),
		DispatchLeaseID:   "lease-leased-waitpoint",
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
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

func TestRespondWaitpointResponseTokenResolvesSingleResponse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
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
		CompletedByPrincipal: pgvalue.Text("reviewer@example.com"),
		CompletedVia:         pgvalue.Text("email_token"),
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
		ResolutionKind: pgvalue.Text("completed"),
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

func TestResolveWaitpointRecordsAndResolvesSingleResponse(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "api-single-response")
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		ResponseKey:          "user:admin",
		Action:               "respond",
		ResolutionKind:       pgvalue.Text("completed"),
		Resolution:           approvedWaitpointResolution("admin"),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgvalue.Text("admin"),
		CompletedVia:         pgvalue.Text("authenticated_api"),
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
		ResolutionKind: pgvalue.Text("completed"),
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

func TestListPendingWaitpointsForRunsMatchesSingleRunQuery(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "pending-batch-match")
	secondRunID, secondWaitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "pending-batch-match-second")

	single, err := queries.GetPendingWaitpointForRun(ctx, db.GetPendingWaitpointForRunParams{
		OrgID: orgID,
		RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := queries.ListPendingWaitpointsForRuns(ctx, db.ListPendingWaitpointsForRunsParams{
		OrgID:  orgID,
		RunIds: []pgtype.UUID{runID, secondRunID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 {
		t.Fatalf("batch = %+v, want two pending waitpoints", batch)
	}
	batchByRunID := map[pgtype.UUID]db.ListPendingWaitpointsForRunsRow{}
	for _, row := range batch {
		batchByRunID[row.RunID] = row
	}
	got, ok := batchByRunID[runID]
	if !ok {
		t.Fatalf("batch missing run %v: %+v", runID, batch)
	}
	if got.ID != single.ID || got.ID != waitpointID || got.RunWaitID != single.RunWaitID || got.RunID != single.RunID || got.Kind != single.Kind || got.DisplayText != single.DisplayText {
		t.Fatalf("batch waitpoint = %+v, single = %+v", got, single)
	}
	if got.RequestedAt != single.RequestedAt || got.CreatedAt != single.CreatedAt {
		t.Fatalf("batch times = requested %+v created %+v, single requested %+v created %+v", got.RequestedAt, got.CreatedAt, single.RequestedAt, single.CreatedAt)
	}
	secondGot, ok := batchByRunID[secondRunID]
	if !ok {
		t.Fatalf("batch missing second run %v: %+v", secondRunID, batch)
	}
	if secondGot.ID != secondWaitpointID || secondGot.RunID != secondRunID {
		t.Fatalf("second batch waitpoint = %+v, want run %v waitpoint %v", secondGot, secondRunID, secondWaitpointID)
	}
}

func TestResolveWaitpointRequiresSuspendedQueueEntryBeforeMutating(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "api-missing-suspended")
	if _, err := queries.RecordWaitpointResponse(ctx, db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		ResponseKey:          "user:admin",
		Action:               "respond",
		ResolutionKind:       pgvalue.Text("completed"),
		Resolution:           []byte(`{"value":{"approved":true}}`),
		EventPayload:         []byte(`{"resolution_kind":"completed"}`),
		CompletedByPrincipal: pgvalue.Text("admin"),
		CompletedVia:         pgvalue.Text("authenticated_api"),
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)

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
	orgID := ids.ToPG(dbtest.DefaultOrgID)
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
	orgID := ids.ToPG(dbtest.DefaultOrgID)
	runID, waitpointID := seedWaitingWaitpoint(t, ctx, pool, queries, orgID, "delivery-stale-sent")
	deliveryID := ids.ToPG(ids.New())
	future := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := queries.CreateQueuedWaitpointEmailDelivery(ctx, db.CreateQueuedWaitpointEmailDeliveryParams{
		DeliveryID:       deliveryID,
		OrgID:            orgID,
		RunID:            runID,
		WaitpointID:      waitpointID,
		TokenHash:        []byte{1},
		ExpiresAt:        pgvalue.Timestamptz(future),
		Recipient:        "owner@example.test",
		TokenMetadata:    []byte(`{}`),
		MessageID:        pgvalue.Text("<waitpoint-delivery@example.test>"),
		DeliveryMetadata: []byte(`{"source":"test"}`),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimWaitpointDeliveryForSend(ctx, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.RequeueStaleSendingWaitpointDeliveries(ctx, db.RequeueStaleSendingWaitpointDeliveriesParams{
		StaleBefore: pgvalue.Timestamptz(future),
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
