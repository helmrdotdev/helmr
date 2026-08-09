package dispatch

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

const dispatchMeasurementEnabled = "HELMR_MEASURE_DISPATCH"

func seedDispatchMeasurement(t *testing.T, fixture runPlacementFixture, rows, scopes, ineligibleEvery int, skewed bool) {
	t.Helper()
	if rows < 1 || scopes < 1 || scopes > rows {
		t.Fatalf("invalid measurement shape rows=%d scopes=%d", rows, scopes)
	}

	var taskDefinitionID uuid.UUID
	var workspaceDefinitionID uuid.UUID
	if err := fixture.pool.QueryRow(fixture.ctx, `
SELECT task.id, sandbox.id
  FROM deployment_definitions AS task
  JOIN deployment_definitions AS sandbox
    ON sandbox.environment_id = task.environment_id
   AND sandbox.deployment_id = task.deployment_id
 WHERE task.environment_id = $1
   AND task.kind = 'task'
   AND sandbox.kind = 'sandbox'`, fixture.environmentID).Scan(&taskDefinitionID, &workspaceDefinitionID); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	queue, concurrency := dispatchMeasurementScope(0, rows, scopes, skewed)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE runs
   SET queue_name = $2,
       concurrency_key = $3,
       priority = 0,
       queue_origin_at = $4,
       queue_score_at = $4,
       queued_expires_at = NULL
 WHERE id = $1`, fixture.runID, queue, concurrency, base)

	tx, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(fixture.ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}

	workspaces := make([][]any, 0, rows-1)
	versions := make([][]any, 0, rows-1)
	runs := make([][]any, 0, rows-1)
	attempts := make([][]any, 0, rows-1)
	for index := 1; index < rows; index++ {
		runID := measurementUUID("run", index)
		workspaceID := measurementUUID("workspace", index)
		versionID := measurementUUID("version", index)
		queueName, concurrencyKey := dispatchMeasurementScope(index, rows, scopes, skewed)
		priority := index % 11
		origin := base.Add(time.Duration(index) * time.Millisecond)
		score := origin.Add(-time.Duration(priority) * time.Second)
		var expiresAt any
		if ineligibleEvery > 0 && index%ineligibleEvery == 0 {
			expiresAt = base.Add(-time.Minute)
		}
		traceID := fmt.Sprintf("%032x", index+1)
		rootSpanID := fmt.Sprintf("%016x", index+1)

		workspaces = append(workspaces, []any{
			workspaceID, fixture.environmentID, "us-east-1", "test-workspace",
			workspaceDefinitionID, runID, int64(1), int64(0), versionID,
		})
		versions = append(versions, []any{
			versionID, fixture.environmentID, workspaceID, "system",
			"sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96",
			"committed", int64(0), int64(0), base,
		})
		runs = append(runs, []any{
			runID, fixture.orgID, fixture.projectID, fixture.environmentID,
			fixture.deploymentID, taskDefinitionID, "task", "test-task", "api",
			workspaceID, versionID, `{}`, queueName, concurrencyKey, int32(priority),
			origin, score, expiresAt, int64(300_000), `{"enabled":false}`, traceID, rootSpanID,
		})
		attempts = append(attempts, []any{runID, int32(1), "task", workspaceID, versionID})
	}

	copyRows(t, fixture.ctx, tx, "workspaces", []string{
		"id", "environment_id", "region_id", "sandbox_declared_id", "deployment_definition_id",
		"owner_run_id", "ownership_generation", "writer_generation", "head_version_id",
	}, workspaces)
	copyRows(t, fixture.ctx, tx, "workspace_versions", []string{
		"id", "environment_id", "workspace_id", "kind", "content_digest", "state",
		"ownership_generation", "writer_generation", "published_at",
	}, versions)
	copyRows(t, fixture.ctx, tx, "runs", []string{
		"id", "org_id", "project_id", "environment_id", "deployment_id",
		"deployment_definition_id", "entrypoint_kind", "entrypoint_declared_id", "cause_kind",
		"workspace_id", "base_workspace_version_id", "payload", "queue_name", "concurrency_key", "priority",
		"queue_origin_at", "queue_score_at", "queued_expires_at", "max_active_duration_ms", "retry_policy",
		"trace_id", "root_span_id",
	}, runs)
	copyRows(t, fixture.ctx, tx, "run_attempts", []string{
		"run_id", "number", "entrypoint_kind", "workspace_id", "base_workspace_version_id",
	}, attempts)
	if err := tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `ANALYZE runs`)
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `ANALYZE workspaces`)
}

func copyRows(t *testing.T, ctx context.Context, tx pgx.Tx, table string, columns []string, rows [][]any) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{table}, columns, pgx.CopyFromRows(rows)); err != nil {
		t.Fatalf("copy %s: %v", table, err)
	}
}

func measurementUUID(kind string, index int) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("helmr-dispatch-measure:%s:%d", kind, index)))
}

func dispatchMeasurementScope(index, rows, scopes int, skewed bool) (string, any) {
	scope := index % scopes
	if skewed && scopes > 1 {
		if index < rows*8/10 {
			scope = 0
		} else {
			scope = 1 + index%(scopes-1)
		}
	}
	queue := fmt.Sprintf("measure-%04d", scope)
	if scope%2 == 0 {
		return queue, nil
	}
	return queue, fmt.Sprintf("key-%04d", scope)
}

func percentileIndex(length, percentile int) int {
	return max(0, (length*percentile+99)/100-1)
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}
