package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

const dispatchMeasurementEnabled = "HELMR_MEASURE_DISPATCH"

type dispatchMeasurement struct {
	Scenario             string  `json:"scenario"`
	Rows                 int     `json:"rows"`
	RequestedScopes      int     `json:"requested_scopes"`
	EligibleScopes       int     `json:"eligible_scopes"`
	Candidates           int     `json:"candidates"`
	Statements           int     `json:"statements"`
	MinimumMillis        float64 `json:"minimum_millis"`
	MedianMillis         float64 `json:"median_millis"`
	MaximumMillis        float64 `json:"maximum_millis"`
	PlacementMillis      float64 `json:"placement_millis,omitempty"`
	PlacementAttempts    int     `json:"placement_attempts,omitempty"`
	SuccessfulPlacements int     `json:"successful_placements,omitempty"`
}

type dispatchPlanMeasurement struct {
	Scenario            string   `json:"scenario"`
	Query               string   `json:"query"`
	PlanningMillis      float64  `json:"planning_millis"`
	ExecutionMillis     float64  `json:"execution_millis"`
	SerializationMillis float64  `json:"serialization_millis"`
	SharedHitBlocks     float64  `json:"shared_hit_blocks"`
	SharedReadBlocks    float64  `json:"shared_read_blocks"`
	TempReadBlocks      float64  `json:"temp_read_blocks"`
	TempWrittenBlocks   float64  `json:"temp_written_blocks"`
	SortMethods         []string `json:"sort_methods,omitempty"`
	MaximumSortSpaceKiB float64  `json:"maximum_sort_space_kib,omitempty"`
}

func TestMeasureDispatchDiscovery(t *testing.T) {
	if os.Getenv(dispatchMeasurementEnabled) != "1" {
		t.Skip(dispatchMeasurementEnabled + "=1 is required")
	}

	scenarios := []struct {
		name            string
		rows            int
		scopes          int
		ineligibleEvery int
		skewed          bool
		place           bool
	}{
		{name: "deep_scope_1000", rows: 1_000, scopes: 1, ineligibleEvery: 10},
		{name: "deep_scope_10000", rows: 10_000, scopes: 1, ineligibleEvery: 10},
		{name: "deep_scope_100000", rows: 100_000, scopes: 1, ineligibleEvery: 10},
		{name: "many_scopes_1000", rows: 1_000, scopes: 256, ineligibleEvery: 10},
		{name: "many_scopes_10000", rows: 10_000, scopes: 256, ineligibleEvery: 10},
		{name: "many_scopes_100000", rows: 100_000, scopes: 256, ineligibleEvery: 10},
		{name: "skewed_scopes_10000", rows: 10_000, scopes: 256, ineligibleEvery: 10, skewed: true},
		{name: "placement_cycle", rows: 96, scopes: 32, ineligibleEvery: 10, place: true},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newRunPlacementFixtureWithSeed(t, "dispatch-measurement:"+scenario.name)
			seedDispatchMeasurement(t, fixture, scenario.rows, scenario.scopes, scenario.ineligibleEvery, scenario.skewed)
			measurement := measureDispatchDiscovery(t, fixture, scenario.name, scenario.rows, scenario.scopes)
			if scenario.place {
				measurement.PlacementMillis, measurement.PlacementAttempts, measurement.SuccessfulPlacements =
					measurePlacementCycle(t, fixture)
			}
			payload, err := json.Marshal(measurement)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("dispatch_measurement=%s", payload)
		})
	}
}

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

func measureDispatchDiscovery(
	t *testing.T,
	fixture runPlacementFixture,
	name string,
	rows int,
	requestedScopes int,
) dispatchMeasurement {
	t.Helper()
	connection, err := fixture.pool.Acquire(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	queries := db.New(connection)
	const iterations = 12
	durations := make([]time.Duration, 0, iterations)
	var eligibleScopes int
	var candidates int
	var statements int
	var scopeParams db.ListQueuedRunEligibleScopesParams
	var candidateParams db.ListQueuedRunDispatchCandidatesForScopesParams

	for iteration := 0; iteration < iterations+3; iteration++ {
		start := time.Now()
		scopeParams = db.ListQueuedRunEligibleScopesParams{
			RowLimit: 32,
			ScanSeed: "dispatch-measurement",
		}
		scopes, err := queries.ListQueuedRunEligibleScopes(fixture.ctx, scopeParams)
		if err != nil {
			t.Fatal(err)
		}
		queueScopes := make([]QueueScope, 0, len(scopes))
		for _, scope := range scopes {
			queueScopes = append(queueScopes, QueueScope{
				OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
				RegionID: scope.RegionID, ConcurrencyKey: scope.ConcurrencyKey, QueueName: scope.QueueName,
			})
		}
		candidateParams, err = runCandidateParams(queueScopes, 32)
		if err != nil {
			t.Fatal(err)
		}
		found, err := queries.ListQueuedRunDispatchCandidatesForScopes(fixture.ctx, candidateParams)
		if err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		if iteration >= 3 {
			durations = append(durations, elapsed)
		}
		eligibleScopes = len(scopes)
		candidates = len(found)
		statements = 2
	}

	slices.Sort(durations)
	if os.Getenv("HELMR_MEASURE_DISPATCH_EXPLAIN") == "1" {
		logDispatchPlan(t, connection.Conn(), name, "eligible_scopes", "-- name: ListQueuedRunEligibleScopes :many", []any{
			scopeParams.AfterSortKey, scopeParams.AfterOrgID, scopeParams.AfterProjectID,
			scopeParams.AfterEnvironmentID, scopeParams.AfterRegionID, scopeParams.AfterConcurrencyKey,
			scopeParams.AfterQueueName, scopeParams.RowLimit, scopeParams.ScanSeed, scopeParams.RegionFilter,
		})
		logDispatchPlan(t, connection.Conn(), name, "candidates", "-- name: ListQueuedRunDispatchCandidatesForScopes :many", []any{
			candidateParams.PerScopeLimit, candidateParams.OrgIds, candidateParams.ProjectIds,
			candidateParams.EnvironmentIds, candidateParams.RegionIds, candidateParams.ConcurrencyKeys,
			candidateParams.QueueNames,
		})
	}
	return dispatchMeasurement{
		Scenario: name, Rows: rows, RequestedScopes: requestedScopes,
		EligibleScopes: eligibleScopes, Candidates: candidates, Statements: statements,
		MinimumMillis: milliseconds(durations[0]), MedianMillis: milliseconds(durations[len(durations)/2]),
		MaximumMillis: milliseconds(durations[len(durations)-1]),
	}
}

func logDispatchPlan(t *testing.T, connection *pgx.Conn, scenario, query, prefix string, args []any) {
	t.Helper()
	var statement string
	if err := connection.QueryRow(
		context.Background(),
		`SELECT statement FROM pg_prepared_statements WHERE statement LIKE $1 ORDER BY name LIMIT 1`,
		prefix+"%",
	).Scan(&statement); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := connection.QueryRow(
		context.Background(),
		"EXPLAIN (ANALYZE, BUFFERS, SERIALIZE BINARY, SETTINGS, MEMORY, FORMAT JSON) "+statement,
		args...,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}

	var document []map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 1 {
		t.Fatalf("EXPLAIN returned %d documents", len(document))
	}
	measurement := dispatchPlanMeasurement{
		Scenario: scenario, Query: query,
		PlanningMillis: number(document[0]["Planning Time"]), ExecutionMillis: number(document[0]["Execution Time"]),
		SerializationMillis: number(document[0]["Serialization Time"]),
	}
	plan, ok := document[0]["Plan"].(map[string]any)
	if !ok {
		t.Fatal("EXPLAIN plan is missing")
	}
	measurement.SharedHitBlocks = number(plan["Shared Hit Blocks"])
	measurement.SharedReadBlocks = number(plan["Shared Read Blocks"])
	measurement.TempReadBlocks = number(plan["Temp Read Blocks"])
	measurement.TempWrittenBlocks = number(plan["Temp Written Blocks"])
	collectSortMeasurements(plan, &measurement)
	payload, err := json.Marshal(measurement)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dispatch_plan=%s", payload)

	if directory := os.Getenv("HELMR_DISPATCH_MEASURE_PLAN_DIR"); directory != "" {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("%s/%s-%s.json", directory, scenario, query)
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func collectSortMeasurements(plan map[string]any, measurement *dispatchPlanMeasurement) {
	if method, ok := plan["Sort Method"].(string); ok {
		measurement.SortMethods = append(measurement.SortMethods, method)
		measurement.MaximumSortSpaceKiB = max(measurement.MaximumSortSpaceKiB, number(plan["Sort Space Used"]))
	}
	children, _ := plan["Plans"].([]any)
	for _, child := range children {
		if childPlan, ok := child.(map[string]any); ok {
			collectSortMeasurements(childPlan, measurement)
		}
	}
}

func number(value any) float64 {
	result, _ := value.(float64)
	return result
}

func measurePlacementCycle(t *testing.T, fixture runPlacementFixture) (float64, int, int) {
	t.Helper()
	dbtest.MustExec(t, fixture.ctx, fixture.pool, `
UPDATE worker_instances
   SET epoch_cpu_millis = 128000,
       epoch_memory_bytes = 137438953472,
       epoch_guest_ephemeral_disk_bytes = 4398046511104,
       max_vm_slots = 128,
       max_runtime_starts = 128
 WHERE id = $1`, fixture.workerID)

	queries := db.New(fixture.pool)
	scopes, err := queries.ListQueuedRunEligibleScopes(fixture.ctx, db.ListQueuedRunEligibleScopesParams{
		RowLimit: 32,
		ScanSeed: "dispatch-measurement",
	})
	if err != nil {
		t.Fatal(err)
	}
	queueScopes := make([]QueueScope, 0, len(scopes))
	for _, scope := range scopes {
		queueScopes = append(queueScopes, QueueScope{
			OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			RegionID: scope.RegionID, ConcurrencyKey: scope.ConcurrencyKey, QueueName: scope.QueueName,
		})
	}
	params, err := runCandidateParams(queueScopes, 1)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := queries.ListQueuedRunDispatchCandidatesForScopes(fixture.ctx, params)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	succeeded := 0
	for _, candidate := range candidates {
		_, err := fixture.authority.PlaceReadyRun(fixture.ctx, ReadyRunCandidate{
			OrgID: candidate.OrgID, RunID: candidate.RunID, ExpectedRunStateVersion: candidate.StateVersion,
		})
		if err == nil {
			succeeded++
		}
	}
	return milliseconds(time.Since(start)), len(candidates), succeeded
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}
