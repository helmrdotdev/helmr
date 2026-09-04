package db_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	telemetryDeploymentIndexName = "telemetry_outbox_deployment_id_idx"
	telemetryRunAttemptIndexName = "telemetry_outbox_run_attempt_number_idx"
	telemetryDeploymentIndexDDL  = `
CREATE INDEX telemetry_outbox_deployment_id_idx ON telemetry_outbox(deployment_id)
    WHERE deployment_id IS NOT NULL`
	telemetryRunAttemptIndexDDL = `
CREATE INDEX telemetry_outbox_run_attempt_number_idx ON telemetry_outbox(org_id, run_id, attempt_number, id)
    WHERE attempt_number IS NOT NULL`
)

var errTelemetryQueryCaptured = errors.New("telemetry production query captured")

type telemetryProductionQuery struct {
	name            string
	query           string
	args            []any
	expectedIndexes []string
}

type telemetryQueryCapture struct {
	query string
	args  []any
}

func (capture *telemetryQueryCapture) record(query string, args []any) {
	capture.query = query
	capture.args = append([]any(nil), args...)
}

func (capture *telemetryQueryCapture) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	capture.record(query, args)
	return pgconn.CommandTag{}, errTelemetryQueryCaptured
}

func (capture *telemetryQueryCapture) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	capture.record(query, args)
	return nil, errTelemetryQueryCaptured
}

func (capture *telemetryQueryCapture) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	capture.record(query, args)
	return telemetryCapturedRow{}
}

type telemetryCapturedRow struct{}

func (telemetryCapturedRow) Scan(...any) error {
	return errTelemetryQueryCaptured
}

type telemetryUnusedPlan struct {
	name            string
	plan            string
	expectedIndexes []string
	buffers         int64
	timeMS          float64
}

type telemetryUnusedEffects struct {
	eventIngestIDs  []int64
	runLogIngestIDs []int64
	liveEventIDs    []int64
	gcRows          int64
	lifecycle       db.GetTelemetryOutboxLifecycleRow
	frontier        db.GetRunTelemetryFrontierRow
}

type telemetryUnusedIndexSizes struct {
	deploymentBytes int64
	runAttemptBytes int64
	totalBytes      int64
}

type telemetryUnusedMeasurement struct {
	plans   []telemetryUnusedPlan
	effects telemetryUnusedEffects
	sizes   telemetryUnusedIndexSizes
}

func measureTelemetryUnusedIndexes(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	orgID pgtype.UUID,
	runID pgtype.UUID,
) {
	t.Helper()

	outer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Rollback(ctx)

	// Shape representative production states without changing the fixture seen by
	// the existing replay and scale-budget assertions. The outer rollback also
	// restores the shipped candidate index set before those assertions continue.
	dbtest.MustExec(t, ctx, outer, `
UPDATE telemetry_outbox
   SET stream_kind = 'event',
       source_kind = 'run',
       source_id = run_id,
       kind = 'run.test',
       message = '',
       payload = '{}'::jsonb,
       content = NULL,
       size_bytes = NULL,
       observed_seq = NULL,
       state = 'pending',
       written_at = NULL,
       next_retry_at = NULL,
       published_at = NULL,
       publish_locked_until = NULL
 WHERE id % 100 < 10
`)
	dbtest.MustExec(t, ctx, outer, `
UPDATE telemetry_outbox
   SET state = 'pending',
       written_at = NULL,
       next_retry_at = NULL
 WHERE stream_kind = 'run_log'
   AND id % 100 >= 10
   AND id % 100 < 20
`)
	dbtest.MustExec(t, ctx, outer, `
UPDATE telemetry_outbox
   SET state = 'written',
       written_at = now() - interval '25 hours'
 WHERE stream_kind = 'run_log'
   AND id % 100 >= 20
   AND id % 100 < 30
`)
	dbtest.MustExec(t, ctx, outer, `ANALYZE telemetry_outbox`)

	queries := captureTelemetryProductionQueries(t, orgID, runID)
	logTelemetryUnusedEnvironment(t, ctx, outer)

	dbtest.MustExec(t, ctx, outer, telemetryDeploymentIndexDDL)
	dbtest.MustExec(t, ctx, outer, telemetryRunAttemptIndexDDL)
	dbtest.MustExec(t, ctx, outer, `ANALYZE telemetry_outbox`)
	baseline := captureTelemetryUnusedMeasurement(t, ctx, outer, queries, orgID, runID)
	assertTelemetryUnusedPlanSet(t, baseline.plans, true)
	logTelemetryUnusedMeasurement(t, "baseline", baseline)

	dbtest.MustExec(t, ctx, outer, `DROP INDEX `+telemetryDeploymentIndexName)
	dbtest.MustExec(t, ctx, outer, `DROP INDEX `+telemetryRunAttemptIndexName)
	dbtest.MustExec(t, ctx, outer, `ANALYZE telemetry_outbox`)
	candidate := captureTelemetryUnusedMeasurement(t, ctx, outer, queries, orgID, runID)
	assertTelemetryUnusedPlanSet(t, candidate.plans, false)
	logTelemetryUnusedMeasurement(t, "candidate", candidate)

	if !reflect.DeepEqual(candidate.effects, baseline.effects) {
		t.Fatalf("telemetry effects changed after removing unused indexes:\nbaseline=%+v\ncandidate=%+v", baseline.effects, candidate.effects)
	}
}

func captureTelemetryProductionQueries(t *testing.T, orgID, runID pgtype.UUID) []telemetryProductionQuery {
	t.Helper()
	return []telemetryProductionQuery{
		captureTelemetryProductionQuery(t, "event ingest claim", []string{"telemetry_outbox_ingest_claim_idx"}, func(queries *db.Queries) {
			_, _ = queries.ClaimEventIngestBatch(context.Background(), db.ClaimEventIngestBatchParams{
				RowLimit: 250, MaxBatchBytes: 8 << 20, LeaseDuration: pgvalue.Interval(30 * time.Second),
			})
		}),
		captureTelemetryProductionQuery(t, "run-log ingest claim", []string{"telemetry_outbox_ingest_claim_idx"}, func(queries *db.Queries) {
			_, _ = queries.ClaimRunLogIngestBatch(context.Background(), db.ClaimRunLogIngestBatchParams{
				RowLimit: 250, MaxBatchBytes: 8 << 20, LeaseDuration: pgvalue.Interval(30 * time.Second),
			})
		}),
		captureTelemetryProductionQuery(t, "live event claim", []string{"telemetry_outbox_publish_ready_idx"}, func(queries *db.Queries) {
			_, _ = queries.ClaimLiveTelemetryOutbox(context.Background(), db.ClaimLiveTelemetryOutboxParams{
				RowLimit: 250, LeaseDuration: pgvalue.Interval(30 * time.Second),
			})
		}),
		captureTelemetryProductionQuery(t, "written-row GC", []string{"telemetry_outbox_written_gc_idx"}, func(queries *db.Queries) {
			_, _ = queries.PruneTelemetryOutboxWritten(context.Background(), db.PruneTelemetryOutboxWrittenParams{
				RetainFor: pgvalue.Interval(24 * time.Hour), RowLimit: 2500,
			})
		}),
		captureTelemetryProductionQuery(t, "lifecycle lookup", []string{"telemetry_outbox_ingest_claim_idx", "telemetry_outbox_written_gc_idx"}, func(queries *db.Queries) {
			_, _ = queries.GetTelemetryOutboxLifecycle(context.Background(), pgvalue.Interval(24*time.Hour))
		}),
		captureTelemetryProductionQuery(t, "Run telemetry frontier", []string{"telemetry_outbox_run_log_observed_idx", "telemetry_outbox_run_id_idx"}, func(queries *db.Queries) {
			_, _ = queries.GetRunTelemetryFrontier(context.Background(), db.GetRunTelemetryFrontierParams{
				OrgID: orgID, RunID: runID, StreamKind: db.TelemetryStreamKindRunLog,
				AfterSeq: 0, ThroughSeq: 0, FilterValues: []string{},
			})
		}),
	}
}

func captureTelemetryProductionQuery(
	t *testing.T,
	name string,
	expectedIndexes []string,
	invoke func(*db.Queries),
) telemetryProductionQuery {
	t.Helper()
	capture := &telemetryQueryCapture{}
	invoke(db.New(capture))
	if capture.query == "" {
		t.Fatalf("%s did not issue a database query", name)
	}
	return telemetryProductionQuery{
		name: name, query: capture.query, args: capture.args, expectedIndexes: expectedIndexes,
	}
}

func captureTelemetryUnusedMeasurement(
	t *testing.T,
	ctx context.Context,
	outer pgx.Tx,
	productionQueries []telemetryProductionQuery,
	orgID pgtype.UUID,
	runID pgtype.UUID,
) telemetryUnusedMeasurement {
	t.Helper()
	plans := make([]telemetryUnusedPlan, 0, len(productionQueries))
	for _, query := range productionQueries {
		plan := telemetryRollbackValue(t, ctx, outer, func(tx pgx.Tx) (string, error) {
			return telemetryExplain(t, ctx, tx, "EXPLAIN (ANALYZE, BUFFERS, SETTINGS, FORMAT TEXT) "+query.query, query.args...), nil
		})
		plans = append(plans, telemetryUnusedPlan{
			name: query.name, plan: plan, expectedIndexes: query.expectedIndexes,
			buffers: telemetryPlanSharedBuffers(t, plan), timeMS: telemetryPlanExecutionMS(t, plan),
		})
	}
	return telemetryUnusedMeasurement{
		plans:   plans,
		effects: captureTelemetryUnusedEffects(t, ctx, outer, orgID, runID),
		sizes:   captureTelemetryUnusedIndexSizes(t, ctx, outer),
	}
}

func captureTelemetryUnusedEffects(
	t *testing.T,
	ctx context.Context,
	outer pgx.Tx,
	orgID pgtype.UUID,
	runID pgtype.UUID,
) telemetryUnusedEffects {
	t.Helper()
	eventIDs := telemetryRollbackValue(t, ctx, outer, func(tx pgx.Tx) ([]int64, error) {
		rows, err := db.New(tx).ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{
			RowLimit: 250, MaxBatchBytes: 8 << 20, LeaseDuration: pgvalue.Interval(30 * time.Second),
		})
		return telemetryEventIDs(rows), err
	})
	runLogIDs := telemetryRollbackValue(t, ctx, outer, func(tx pgx.Tx) ([]int64, error) {
		rows, err := db.New(tx).ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{
			RowLimit: 250, MaxBatchBytes: 8 << 20, LeaseDuration: pgvalue.Interval(30 * time.Second),
		})
		return telemetryRunLogIDs(rows), err
	})
	liveIDs := telemetryRollbackValue(t, ctx, outer, func(tx pgx.Tx) ([]int64, error) {
		rows, err := db.New(tx).ClaimLiveTelemetryOutbox(ctx, db.ClaimLiveTelemetryOutboxParams{
			RowLimit: 250, LeaseDuration: pgvalue.Interval(30 * time.Second),
		})
		return telemetryLiveIDs(rows), err
	})
	gcRows := telemetryRollbackValue(t, ctx, outer, func(tx pgx.Tx) (int64, error) {
		return db.New(tx).PruneTelemetryOutboxWritten(ctx, db.PruneTelemetryOutboxWrittenParams{
			RetainFor: pgvalue.Interval(24 * time.Hour), RowLimit: 2500,
		})
	})
	lifecycle := telemetryRollbackValue(t, ctx, outer, func(tx pgx.Tx) (db.GetTelemetryOutboxLifecycleRow, error) {
		return db.New(tx).GetTelemetryOutboxLifecycle(ctx, pgvalue.Interval(24*time.Hour))
	})
	frontier := telemetryRollbackValue(t, ctx, outer, func(tx pgx.Tx) (db.GetRunTelemetryFrontierRow, error) {
		return db.New(tx).GetRunTelemetryFrontier(ctx, db.GetRunTelemetryFrontierParams{
			OrgID: orgID, RunID: runID, StreamKind: db.TelemetryStreamKindRunLog,
			AfterSeq: 0, ThroughSeq: 0, FilterValues: []string{},
		})
	})
	return telemetryUnusedEffects{
		eventIngestIDs: eventIDs, runLogIngestIDs: runLogIDs, liveEventIDs: liveIDs,
		gcRows: gcRows, lifecycle: lifecycle, frontier: frontier,
	}
}

func telemetryRollbackValue[T any](
	t *testing.T,
	ctx context.Context,
	outer pgx.Tx,
	measure func(pgx.Tx) (T, error),
) T {
	t.Helper()
	tx, err := outer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	value, measureErr := measure(tx)
	rollbackErr := tx.Rollback(ctx)
	if measureErr != nil {
		t.Fatal(measureErr)
	}
	if rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
	return value
}

func captureTelemetryUnusedIndexSizes(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) telemetryUnusedIndexSizes {
	t.Helper()
	var sizes telemetryUnusedIndexSizes
	if err := q.QueryRow(ctx, `
SELECT COALESCE(pg_relation_size(to_regclass($1::text)), 0),
       COALESCE(pg_relation_size(to_regclass($2::text)), 0),
       pg_indexes_size('telemetry_outbox')
`, telemetryDeploymentIndexName, telemetryRunAttemptIndexName).Scan(
		&sizes.deploymentBytes,
		&sizes.runAttemptBytes,
		&sizes.totalBytes,
	); err != nil {
		t.Fatal(err)
	}
	return sizes
}

func assertTelemetryUnusedPlanSet(t *testing.T, plans []telemetryUnusedPlan, baseline bool) {
	t.Helper()
	for _, measured := range plans {
		if strings.Contains(measured.plan, telemetryDeploymentIndexName) || strings.Contains(measured.plan, telemetryRunAttemptIndexName) {
			stage := "candidate"
			if baseline {
				stage = "baseline"
			}
			t.Fatalf("%s %s unexpectedly used a removed index:\n%s", stage, measured.name, measured.plan)
		}
		if strings.Contains(measured.plan, "Seq Scan on telemetry_outbox") {
			t.Fatalf("%s gained a telemetry_outbox sequential scan:\n%s", measured.name, measured.plan)
		}
		retainedIndexUsed := false
		for _, indexName := range measured.expectedIndexes {
			retainedIndexUsed = retainedIndexUsed || strings.Contains(measured.plan, indexName)
		}
		if len(measured.expectedIndexes) > 0 && !retainedIndexUsed {
			t.Fatalf("%s did not retain an appropriate access path from %v:\n%s", measured.name, measured.expectedIndexes, measured.plan)
		}
	}
}

func logTelemetryUnusedEnvironment(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) {
	t.Helper()
	var version, settings string
	if err := q.QueryRow(ctx, `SELECT version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRow(ctx, `
SELECT string_agg(
           name || '=' || setting || CASE WHEN unit <> '' THEN ' ' || unit ELSE '' END,
           ', ' ORDER BY name
       )
  FROM pg_settings
 WHERE name = ANY(ARRAY[
       'block_size', 'effective_cache_size', 'effective_io_concurrency',
       'enable_bitmapscan', 'enable_indexonlyscan', 'enable_indexscan',
       'enable_seqscan', 'jit', 'random_page_cost', 'seq_page_cost',
       'shared_buffers', 'work_mem'
 ])
`).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	t.Logf("telemetry unused-index measurement PostgreSQL: %s", version)
	t.Logf("telemetry unused-index measurement settings: %s", settings)
}

func logTelemetryUnusedMeasurement(t *testing.T, stage string, measurement telemetryUnusedMeasurement) {
	t.Helper()
	t.Logf(
		"telemetry unused-index %s sizes: deployment_index_bytes=%d run_attempt_index_bytes=%d total_outbox_index_bytes=%d",
		stage, measurement.sizes.deploymentBytes, measurement.sizes.runAttemptBytes, measurement.sizes.totalBytes,
	)
	for _, measured := range measurement.plans {
		t.Logf("telemetry unused-index %s %s summary: shared_buffers=%d execution_ms=%.3f", stage, measured.name, measured.buffers, measured.timeMS)
		t.Logf("telemetry unused-index %s %s explain:\n%s", stage, measured.name, measured.plan)
	}
	t.Logf(
		"telemetry unused-index %s effects: event_ingest=%d run_log_ingest=%d live_event=%d written_gc=%d lifecycle=%+v frontier=%+v",
		stage,
		len(measurement.effects.eventIngestIDs),
		len(measurement.effects.runLogIngestIDs),
		len(measurement.effects.liveEventIDs),
		measurement.effects.gcRows,
		measurement.effects.lifecycle,
		measurement.effects.frontier,
	)
}

func telemetryEventIDs(rows []db.ClaimEventIngestBatchRow) []int64 {
	ids := make([]int64, len(rows))
	for index := range rows {
		ids[index] = rows[index].OutboxID
	}
	return ids
}

func telemetryRunLogIDs(rows []db.ClaimRunLogIngestBatchRow) []int64 {
	ids := make([]int64, len(rows))
	for index := range rows {
		ids[index] = rows[index].OutboxID
	}
	return ids
}

func telemetryLiveIDs(rows []db.ClaimLiveTelemetryOutboxRow) []int64 {
	ids := make([]int64, len(rows))
	for index := range rows {
		ids[index] = rows[index].OutboxID
	}
	return ids
}
