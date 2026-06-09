package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestListRunSummariesRunningFilterIncludesRunningRuns(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)

	runningRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	succeededRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	for _, run := range []struct {
		id     pgtype.UUID
		status db.RunStatus
	}{
		{id: runningRunID, status: db.RunStatusRunning},
		{id: succeededRunID, status: db.RunStatusSucceeded},
	} {
		if _, err := pool.Exec(ctx, `
	UPDATE runs
	   SET status = $3::run_status,
	       execution_status = CASE WHEN $3::run_status IN ('succeeded', 'failed', 'cancelled', 'expired') THEN 'finished'::run_execution_status ELSE execution_status END,
	       terminal_outcome = CASE WHEN $3::run_status IN ('succeeded', 'failed', 'cancelled', 'expired') THEN $3::text::run_terminal_outcome ELSE terminal_outcome END,
	       updated_at = now()
 WHERE org_id = $1
   AND id = $2
`, orgID, run.id, run.status); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := queries.ListRunSummaries(ctx, db.ListRunSummariesParams{
		OrgID:        orgID,
		StatusFilter: "running",
		RowLimit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[db.RunStatus]int{}
	for _, row := range rows {
		got[row.Status]++
	}
	if len(rows) != 1 || got[db.RunStatusRunning] != 1 {
		t.Fatalf("running summary statuses = %+v, rows = %+v", got, rows)
	}

	scopedRows, err := queries.ListScopedRunSummaries(ctx, db.ListScopedRunSummariesParams{
		OrgID:         orgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		StatusFilter:  "running",
		RowLimit:      10,
	})
	if err != nil {
		t.Fatal(err)
	}
	got = map[db.RunStatus]int{}
	for _, row := range scopedRows {
		got[row.Status]++
	}
	if len(scopedRows) != 1 || got[db.RunStatusRunning] != 1 {
		t.Fatalf("scoped running summary statuses = %+v, rows = %+v", got, scopedRows)
	}
}

func TestExpireQueuedRunsHandlesMultipleRuns(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	runs := make([]pgtype.UUID, 0, 2)
	for range 2 {
		runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
		if _, err := pool.Exec(ctx, `
UPDATE runs
   SET ttl = '1m',
       queued_expires_at = now() - interval '1 second'
 WHERE org_id = $1
   AND id = $2
`, orgID, runID); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, runID)
	}

	if err := queries.ExpireQueuedRuns(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	for _, runID := range runs {
		requireRunStatus(t, ctx, pool, orgID, runID, db.RunStatusExpired)
		requireRunSnapshotTransitionCount(t, ctx, pool, orgID, runID, "run.expired", 1)
		requireRunEventKindCount(t, ctx, pool, orgID, runID, "run.expired", 1)
	}
}

func TestRunOperationTerminalStateCannotBeOverwritten(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:             ids.ToPG(ids.New()),
		OrgID:          orgID,
		ProjectID:      scope.ProjectID,
		EnvironmentID:  scope.EnvironmentID,
		RunID:          runID,
		Kind:           db.RunOperationKindReplay,
		ActorKind:      "test",
		ActorID:        "db-test",
		Reason:         "replay",
		Request:        []byte(`{"idempotency_key":"same"}`),
		IdempotencyKey: "same",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunOperationApplied(ctx, db.MarkRunOperationAppliedParams{
		OrgID:  orgID,
		ID:     operation.ID,
		Result: []byte(`{"run_id":"00000000-0000-0000-0000-000000000001"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkRunOperationRejected(ctx, db.MarkRunOperationRejectedParams{
		OrgID:  orgID,
		ID:     operation.ID,
		Result: []byte(`{"error":"conflict"}`),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reject applied operation error = %v, want no rows", err)
	}
	got, err := queries.GetRunOperation(ctx, db.GetRunOperationParams{OrgID: orgID, ID: operation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.RunOperationStatusApplied {
		t.Fatalf("operation status = %s, want applied", got.Status)
	}
}

func TestCancelRunRejectsNonCancelOperationWithoutMutation(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	runID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:             ids.ToPG(ids.New()),
		OrgID:          orgID,
		ProjectID:      scope.ProjectID,
		EnvironmentID:  scope.EnvironmentID,
		RunID:          runID,
		Kind:           db.RunOperationKindReplay,
		ActorKind:      "test",
		ActorID:        "db-test",
		Reason:         "replay",
		Request:        []byte(`{"idempotency_key":"same"}`),
		IdempotencyKey: "same",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = queries.CancelRun(ctx, db.CancelRunParams{
		OrgID:       orgID,
		RunID:       runID,
		OperationID: operation.ID,
		Force:       true,
		Reason:      "stop",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CancelRun with replay operation error = %v, want no rows", err)
	}

	var status db.RunStatus
	if err := pool.QueryRow(ctx, `
SELECT status
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusQueued {
		t.Fatalf("run status = %s, want queued", status)
	}
	got, err := queries.GetRunOperation(ctx, db.GetRunOperationParams{OrgID: orgID, ID: operation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.RunOperationStatusRequested {
		t.Fatalf("operation status = %s, want requested", got.Status)
	}
}

func TestCreateScopedRunAllowsReplayOperationOwnedBySourceRun(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	orgID := ids.ToPG(ids.DefaultOrgID)

	scope := seedPostgresTestConfiguredScope(t, ctx, pool, queries, orgID)
	sourceRunID := seedComputeDispatchRun(t, ctx, pool, orgID, scope.ProjectID, scope.EnvironmentID)
	operation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:             ids.ToPG(ids.New()),
		OrgID:          orgID,
		ProjectID:      scope.ProjectID,
		EnvironmentID:  scope.EnvironmentID,
		RunID:          sourceRunID,
		Kind:           db.RunOperationKindReplay,
		ActorKind:      "test",
		ActorID:        "db-test",
		Reason:         "replay",
		Request:        []byte(`{"idempotency_key":"same"}`),
		IdempotencyKey: "same",
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelOperation, err := queries.CreateRunOperation(ctx, db.CreateRunOperationParams{
		ID:             ids.ToPG(ids.New()),
		OrgID:          orgID,
		ProjectID:      scope.ProjectID,
		EnvironmentID:  scope.EnvironmentID,
		RunID:          sourceRunID,
		Kind:           db.RunOperationKindCancel,
		ActorKind:      "test",
		ActorID:        "db-test",
		Reason:         "cancel",
		Request:        []byte(`{"reason":"stop"}`),
		IdempotencyKey: "cancel",
	})
	if err != nil {
		t.Fatal(err)
	}

	var deploymentID pgtype.UUID
	var deploymentTaskID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT deployment_id, deployment_task_id
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, sourceRunID).Scan(&deploymentID, &deploymentTaskID); err != nil {
		t.Fatal(err)
	}
	traceID, err := tracing.NewTraceID()
	if err != nil {
		t.Fatal(err)
	}
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = queries.CreateScopedRun(ctx, db.CreateScopedRunParams{
		ID:                      ids.ToPG(ids.New()),
		OrgID:                   orgID,
		ProjectID:               scope.ProjectID,
		EnvironmentID:           scope.EnvironmentID,
		DeploymentID:            deploymentID,
		DeploymentTaskID:        deploymentTaskID,
		DeploymentVersion:       "v0",
		ApiVersion:              "v0",
		SdkVersion:              "test",
		CliVersion:              "",
		TaskID:                  "deploy",
		Payload:                 []byte(`{"replay":true}`),
		Metadata:                []byte(`{}`),
		Tags:                    []string{"replay"},
		IdempotencyKeyOptions:   []byte(`{}`),
		LockedRetryPolicy:       []byte(`false`),
		ReplayedFromRunID:       sourceRunID,
		ReplayOperationID:       cancelOperation.ID,
		QueueName:               "task/deploy",
		Priority:                0,
		QueueTimestamp:          pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		Ttl:                     "",
		MaxDurationSeconds:      300,
		TraceID:                 traceID,
		RootSpanID:              rootSpanID,
		EventPayload:            []byte(`{"source":"replay"}`),
		IdempotencyKey:          pgtype.Text{},
		IdempotencyKeyExpiresAt: pgtype.Timestamptz{},
		IdempotencyRequestHash:  pgtype.Text{},
	})
	if err == nil {
		t.Fatal("CreateScopedRun with cancel replay_operation_id error = nil, want error")
	}

	replayedRunID := ids.ToPG(ids.New())
	replayed, err := queries.CreateScopedRun(ctx, db.CreateScopedRunParams{
		ID:                      replayedRunID,
		OrgID:                   orgID,
		ProjectID:               scope.ProjectID,
		EnvironmentID:           scope.EnvironmentID,
		DeploymentID:            deploymentID,
		DeploymentTaskID:        deploymentTaskID,
		DeploymentVersion:       "v0",
		ApiVersion:              "v0",
		SdkVersion:              "test",
		CliVersion:              "",
		TaskID:                  "deploy",
		Payload:                 []byte(`{"replay":true}`),
		Metadata:                []byte(`{}`),
		Tags:                    []string{"replay"},
		IdempotencyKeyOptions:   []byte(`{}`),
		LockedRetryPolicy:       []byte(`false`),
		ReplayedFromRunID:       sourceRunID,
		ReplayOperationID:       operation.ID,
		QueueName:               "task/deploy",
		Priority:                0,
		QueueTimestamp:          pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		Ttl:                     "",
		MaxDurationSeconds:      300,
		TraceID:                 traceID,
		RootSpanID:              rootSpanID,
		EventPayload:            []byte(`{"source":"replay"}`),
		IdempotencyKey:          pgtype.Text{},
		IdempotencyKeyExpiresAt: pgtype.Timestamptz{},
		IdempotencyRequestHash:  pgtype.Text{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ReplayedFromRunID != sourceRunID {
		t.Fatalf("replayed_from_run_id = %v, want %v", replayed.ReplayedFromRunID, sourceRunID)
	}

	var storedReplayOperationID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT replay_operation_id
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, replayedRunID).Scan(&storedReplayOperationID); err != nil {
		t.Fatal(err)
	}
	if storedReplayOperationID != operation.ID {
		t.Fatalf("replay_operation_id = %v, want %v", storedReplayOperationID, operation.ID)
	}

	var snapshotOperationID pgtype.UUID
	if err := pool.QueryRow(ctx, `
SELECT operation_id
  FROM run_snapshots
 WHERE org_id = $1
   AND run_id = $2
   AND version = 1
`, orgID, replayedRunID).Scan(&snapshotOperationID); err != nil {
		t.Fatal(err)
	}
	if snapshotOperationID.Valid {
		t.Fatalf("created replay snapshot operation_id valid = true, want false")
	}
	if _, err := pool.Exec(ctx, `
DELETE FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, sourceRunID); err != nil {
		t.Fatal(err)
	}
	var replayedFromRunID pgtype.UUID
	storedReplayOperationID = pgtype.UUID{}
	if err := pool.QueryRow(ctx, `
SELECT replayed_from_run_id, replay_operation_id
  FROM runs
 WHERE org_id = $1
   AND id = $2
`, orgID, replayedRunID).Scan(&replayedFromRunID, &storedReplayOperationID); err != nil {
		t.Fatal(err)
	}
	if replayedFromRunID.Valid || storedReplayOperationID.Valid {
		t.Fatalf("replay links after source delete = (%v, %v), want both null", replayedFromRunID, storedReplayOperationID)
	}
}
