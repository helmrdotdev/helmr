package dispatch

import (
	"context"
	"fmt"
	"testing"
	"time"
	"uuid"

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

	var publisherID uuid.UUID
	var rootDigest string
	var rootBytes int64
	if err := tx.QueryRow(fixture.ctx, `SELECT v.publisher_runtime_instance_id,v.root_pack_digest,v.logical_bytes FROM computer_versions v
JOIN computers w ON w.head_version_id=v.id WHERE w.id=$1`, fixture.workspaceID).Scan(&publisherID, &rootDigest, &rootBytes); err != nil {
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
			versionID, fixture.environmentID, workspaceID,
			rootDigest, rootBytes,
			"committed", int64(0), int64(0), base, versionID, int64(1), dbtest.Hash(versionID.String()),
		})
		runs = append(runs, []any{
			runID, fixture.orgID, fixture.projectID, fixture.environmentID,
			fixture.deploymentID, taskDefinitionID, "task", "test-task", "api",
			workspaceID, versionID, `{}`, queueName, concurrencyKey, int32(priority),
			origin, score, expiresAt, int64(300_000), `{"enabled":false}`, traceID, rootSpanID,
		})
		attempts = append(attempts, []any{runID, int32(1), "task", workspaceID, versionID})
	}

	copyRows(t, fixture.ctx, tx, "computers", []string{
		"id", "environment_id", "region_id", "sandbox_declared_id", "deployment_definition_id",
		"owner_run_id", "ownership_generation", "writer_generation", "head_version_id",
	}, workspaces)
	dbtest.MustExec(t, fixture.ctx, tx, `INSERT INTO runtime_instances(id,org_id,project_id,environment_id,region_id,worker_group_id,worker_instance_id,
 runtime_identity_id,deployment_definition_id,worker_epoch,vm_vcpu_count,cpu_config_digest,reserved_cpu_millis,reserved_memory_bytes,
 reserved_guest_ephemeral_disk_bytes,reserved_execution_slots,workspace_id,preparation_expires_at,desired_state,desired_version,desired_reason,
 observed_state,terminal_at,reclaimed_at,reclaim_evidence,terminal_reason_code)
 SELECT c.head_version_id,r.org_id,r.project_id,c.environment_id,c.region_id,r.worker_group_id,r.worker_instance_id,
 r.runtime_identity_id,c.deployment_definition_id,r.worker_epoch,r.vm_vcpu_count,r.cpu_config_digest,r.reserved_cpu_millis,r.reserved_memory_bytes,
 r.reserved_guest_ephemeral_disk_bytes,r.reserved_execution_slots,c.id,now(),'closed',2,'initialization_completed','closed',now(),now(),'{}','initialization_completed'
 FROM computers c CROSS JOIN runtime_instances r WHERE c.environment_id=$1 AND c.id<>$2 AND r.id=$3`, fixture.environmentID, fixture.workspaceID, publisherID)
	copyRows(t, fixture.ctx, tx, "computer_versions", []string{
		"id", "environment_id", "computer_id", "root_pack_digest", "logical_bytes", "status",
		"ownership_generation", "writer_generation", "published_at", "publisher_runtime_instance_id", "publisher_desired_version", "publication_request_fingerprint",
	}, versions)
	for index := 1; index < rows; index++ {
		insertPlacementGeneration(t, fixture.ctx, tx, fixture.environmentID, measurementUUID("workspace", index), measurementUUID("version", index))
	}
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
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `ANALYZE computers`)
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
	return deterministicUUID(fmt.Sprintf("helmr-dispatch-measure:%s:%d", kind, index))
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
