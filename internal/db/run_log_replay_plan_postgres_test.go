package db_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	telemetryReplayIndexName = "telemetry_outbox_run_log_replay_idx"
	telemetryOldLeaseIndex   = "telemetry_outbox_run_lease_idx"
	telemetryReplayIndexDDL  = `
CREATE INDEX telemetry_outbox_run_log_replay_idx
    ON telemetry_outbox(run_lease_id, stream_name, observed_seq, id)
    WHERE stream_kind = 'run_log' AND source_kind = 'run'`
	telemetryOldLeaseIndexDDL = `
CREATE INDEX telemetry_outbox_run_lease_idx
    ON telemetry_outbox(org_id, run_id, run_lease_id, id)
    WHERE run_lease_id IS NOT NULL`

	telemetryReplaySelect = `
SELECT org_id,
       run_id,
       run_lease_id,
       attempt_number,
       stream_name AS stream,
       id AS seq,
       observed_seq,
       content,
       size_bytes,
       COALESCE(payload ->> 'event_payload', '')::text AS event_payload,
       COALESCE(payload ->> 'lease_fence_fingerprint', '')::text AS lease_fence_fingerprint,
       created_at
  FROM telemetry_outbox
 WHERE stream_kind = 'run_log'
   AND source_kind = 'run'
   AND run_lease_id = $1
   AND stream_name = $2
   AND observed_seq = $3
 ORDER BY id
 LIMIT 1`
)

type telemetryReplayScaleFixture struct {
	leaseIDs []pgtype.UUID
}

type telemetryReplayMeasurement struct {
	hitPlan     string
	missPlan    string
	hitBuffers  int64
	missBuffers int64
	hitRemoved  int64
	missRemoved int64
	hitTimeMS   float64
	missTimeMS  float64
	indexBytes  int64
	totalBytes  int64
	hit         db.GetRunLogChunkReplayRow
}

func TestRunLogReplayPlanUsesBoundedIndexShape(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	plan := telemetryExplain(
		t,
		ctx,
		tx,
		"EXPLAIN (COSTS OFF, FORMAT TEXT) "+telemetryReplaySelect,
		pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		"stdout",
		int64(1),
	)
	assertReplayPlanShape(t, plan)
}

func measureTelemetryReplayIndexes(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	fixture telemetryReplayScaleFixture,
) {
	t.Helper()
	if len(fixture.leaseIDs) == 0 {
		t.Fatal("invalid telemetry replay scale fixture")
	}

	var version string
	if err := pool.QueryRow(ctx, `SELECT version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var settings string
	if err := pool.QueryRow(ctx, `
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
	t.Logf("telemetry replay measurement PostgreSQL: %s", version)
	t.Logf("telemetry replay measurement settings: %s", settings)
	t.Logf(
		"telemetry replay seed: rows=1000000 leases=%d streams=3 distribution=round-robin lease then stream; observed_sequences_per_lease_stream=5208-5209",
		len(fixture.leaseIDs),
	)

	const hitGenerated = int64(777777)
	leaseIndex := (hitGenerated - 1) % int64(len(fixture.leaseIDs))
	streamIndex := ((hitGenerated - 1) / int64(len(fixture.leaseIDs))) % 3
	stream := []string{"stdout", "stderr", "structured"}[streamIndex]
	observedSeq := ((hitGenerated - 1) / (int64(len(fixture.leaseIDs)) * 3)) + 1
	hitLeaseID := fixture.leaseIDs[leaseIndex]
	const missObservedSeq = int64(1_000_001)

	replaceTelemetryReplayIndex(t, ctx, pool, false)
	baseline := captureTelemetryReplayMeasurement(
		t, ctx, pool, telemetryOldLeaseIndex, hitLeaseID, stream, observedSeq, missObservedSeq,
	)
	t.Logf(
		"telemetry replay baseline summary: index_bytes=%d total_outbox_index_bytes=%d hit_shared_buffers=%d hit_rows_removed=%d hit_execution_ms=%.3f miss_shared_buffers=%d miss_rows_removed=%d miss_execution_ms=%.3f",
		baseline.indexBytes,
		baseline.totalBytes,
		baseline.hitBuffers,
		baseline.hitRemoved,
		baseline.hitTimeMS,
		baseline.missBuffers,
		baseline.missRemoved,
		baseline.missTimeMS,
	)
	t.Logf("telemetry replay baseline hit explain:\n%s", baseline.hitPlan)
	t.Logf("telemetry replay baseline miss explain:\n%s", baseline.missPlan)

	replaceTelemetryReplayIndex(t, ctx, pool, true)
	candidate := captureTelemetryReplayMeasurement(
		t, ctx, pool, telemetryReplayIndexName, hitLeaseID, stream, observedSeq, missObservedSeq,
	)
	assertReplayPlanShape(t, candidate.hitPlan)
	assertReplayPlanShape(t, candidate.missPlan)
	if !reflect.DeepEqual(candidate.hit, baseline.hit) {
		t.Fatalf(
			"replay hit changed: baseline=%+v candidate=%+v",
			baseline.hit,
			candidate.hit,
		)
	}
	const maxCandidateSharedBuffers = int64(64)
	if candidate.hitBuffers > maxCandidateSharedBuffers || candidate.missBuffers > maxCandidateSharedBuffers {
		t.Fatalf(
			"candidate replay buffers are not bounded: hit=%d miss=%d maximum=%d",
			candidate.hitBuffers,
			candidate.missBuffers,
			maxCandidateSharedBuffers,
		)
	}
	t.Logf(
		"telemetry replay candidate summary: index_bytes=%d total_outbox_index_bytes=%d hit_shared_buffers=%d hit_rows_removed=%d hit_execution_ms=%.3f miss_shared_buffers=%d miss_rows_removed=%d miss_execution_ms=%.3f",
		candidate.indexBytes,
		candidate.totalBytes,
		candidate.hitBuffers,
		candidate.hitRemoved,
		candidate.hitTimeMS,
		candidate.missBuffers,
		candidate.missRemoved,
		candidate.missTimeMS,
	)
	t.Logf("telemetry replay candidate hit explain:\n%s", candidate.hitPlan)
	t.Logf("telemetry replay candidate miss explain:\n%s", candidate.missPlan)

	fkPlan := telemetryExplain(t, ctx, pool, `
EXPLAIN (SETTINGS, FORMAT TEXT)
SELECT 1
  FROM ONLY telemetry_outbox AS child
 WHERE child.org_id = $1
   AND child.run_id = $2
   AND child.run_lease_id = $3
   FOR KEY SHARE OF child
`, baseline.hit.OrgID, baseline.hit.RunID, hitLeaseID)
	t.Logf("telemetry Run Lease FK reverse-check explain:\n%s", fkPlan)
}

func replaceTelemetryReplayIndex(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	candidate bool,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+telemetryReplayIndexName); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+telemetryOldLeaseIndex); err != nil {
		t.Fatal(err)
	}
	definition := telemetryOldLeaseIndexDDL
	if candidate {
		definition = telemetryReplayIndexDDL
	}
	if _, err := pool.Exec(ctx, definition); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, pool, `VACUUM (ANALYZE) telemetry_outbox`)
}

func captureTelemetryReplayMeasurement(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	indexName string,
	runLeaseID pgtype.UUID,
	stream string,
	observedSeq int64,
	missObservedSeq int64,
) telemetryReplayMeasurement {
	t.Helper()
	queries := db.New(pool)
	hit, err := queries.GetRunLogChunkReplay(ctx, db.GetRunLogChunkReplayParams{
		RunLeaseID: runLeaseID,
		Stream:     stream,
		ObservedSeq: pgtype.Int8{
			Int64: observedSeq,
			Valid: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, missErr := queries.GetRunLogChunkReplay(ctx, db.GetRunLogChunkReplayParams{
		RunLeaseID: runLeaseID,
		Stream:     stream,
		ObservedSeq: pgtype.Int8{
			Int64: missObservedSeq,
			Valid: true,
		},
	})
	if !errors.Is(missErr, pgx.ErrNoRows) {
		t.Fatalf("replay miss error = %v, want no rows", missErr)
	}

	hitPlan := telemetryExplain(
		t,
		ctx,
		pool,
		"EXPLAIN (ANALYZE, BUFFERS, SETTINGS, FORMAT TEXT) "+telemetryReplaySelect,
		runLeaseID,
		stream,
		observedSeq,
	)
	missPlan := telemetryExplain(
		t,
		ctx,
		pool,
		"EXPLAIN (ANALYZE, BUFFERS, SETTINGS, FORMAT TEXT) "+telemetryReplaySelect,
		runLeaseID,
		stream,
		missObservedSeq,
	)
	var indexBytes, totalBytes int64
	if err := pool.QueryRow(ctx, `
SELECT pg_relation_size($1::regclass), pg_indexes_size('telemetry_outbox')
`, indexName).Scan(&indexBytes, &totalBytes); err != nil {
		t.Fatal(err)
	}
	return telemetryReplayMeasurement{
		hitPlan:     hitPlan,
		missPlan:    missPlan,
		hitBuffers:  telemetryPlanSharedBuffers(t, hitPlan),
		missBuffers: telemetryPlanSharedBuffers(t, missPlan),
		hitRemoved:  telemetryPlanRowsRemoved(t, hitPlan),
		missRemoved: telemetryPlanRowsRemoved(t, missPlan),
		hitTimeMS:   telemetryPlanExecutionMS(t, hitPlan),
		missTimeMS:  telemetryPlanExecutionMS(t, missPlan),
		indexBytes:  indexBytes,
		totalBytes:  totalBytes,
		hit:         hit,
	}
}

type telemetryPlanQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func telemetryExplain(
	t *testing.T,
	ctx context.Context,
	q telemetryPlanQuerier,
	query string,
	args ...any,
) string {
	t.Helper()
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func assertReplayPlanShape(t *testing.T, plan string) {
	t.Helper()
	if !strings.Contains(plan, telemetryReplayIndexName) ||
		strings.Contains(plan, "Seq Scan on telemetry_outbox") ||
		strings.Contains(plan, "Sort") {
		t.Fatalf("replay plan is not served directly by %s:\n%s", telemetryReplayIndexName, plan)
	}
}

func telemetryPlanSharedBuffers(t *testing.T, plan string) int64 {
	t.Helper()
	for _, line := range strings.Split(plan, "\n") {
		marker := strings.Index(line, "Buffers: shared ")
		if marker == -1 {
			continue
		}
		var total int64
		for _, field := range strings.Fields(line[marker+len("Buffers: shared "):]) {
			name, value, found := strings.Cut(field, "=")
			if !found || (name != "hit" && name != "read" && name != "dirtied" && name != "written") {
				continue
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("parse shared buffer field %q in plan:\n%s", field, plan)
			}
			total += parsed
		}
		return total
	}
	t.Fatalf("plan did not report shared buffers:\n%s", plan)
	return 0
}

func telemetryPlanExecutionMS(t *testing.T, plan string) float64 {
	t.Helper()
	for _, line := range strings.Split(plan, "\n") {
		const prefix = "Execution Time: "
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimSuffix(strings.TrimPrefix(line, prefix), " ms")
		milliseconds, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("parse execution time %q in plan:\n%s", value, plan)
		}
		return milliseconds
	}
	t.Fatalf("plan did not report execution time:\n%s", plan)
	return 0
}

func telemetryPlanRowsRemoved(t *testing.T, plan string) int64 {
	t.Helper()
	const marker = "Rows Removed by Filter: "
	var total int64
	for _, line := range strings.Split(plan, "\n") {
		index := strings.Index(line, marker)
		if index == -1 {
			continue
		}
		removed, err := strconv.ParseInt(strings.TrimSpace(line[index+len(marker):]), 10, 64)
		if err != nil {
			t.Fatalf("parse rows removed in plan:\n%s", plan)
		}
		total += removed
	}
	return total
}
