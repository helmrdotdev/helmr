package db_test

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestTelemetryOutboxGCScaleBudget(t *testing.T) {
	if os.Getenv("HELMR_TEST_TELEMETRY_GC_SCALE") != "1" {
		t.Skip("HELMR_TEST_TELEMETRY_GC_SCALE is not set")
	}
	ctx := t.Context()

	pool := newPostgresDB(t, ctx)
	ids := seedPostgres(t, ctx, pool)
	queries := db.New(pool)
	start := time.Now()
	dbtest.MustExec(t, ctx, pool, `INSERT INTO telemetry_outbox(environment_id,deployment_id,stream_kind,kind,written_at,published_at) SELECT $1,$2,'event','deployment.ready',CASE WHEN n<=100000 THEN now()-interval '25 hours' ELSE now()-interval '23 hours' END,now() FROM generate_series(1,1000000)n`, ids.environmentID, ids.deploymentID)
	dbtest.MustExec(t, ctx, pool, `ANALYZE telemetry_outbox`)
	t.Logf("seeded 1,000,000 typed events, 100,000 eligible, in %s", time.Since(start))

	var tableBytes, indexBytes int64
	if err := pool.QueryRow(ctx, `
		SELECT pg_table_size('telemetry_outbox'), pg_relation_size('telemetry_outbox_event_gc')
	`).Scan(&tableBytes, &indexBytes); err != nil {
		t.Fatal(err)
	}
	var tempFilesBefore, tempBytesBefore int64
	if err := pool.QueryRow(ctx, `
		SELECT temp_files, temp_bytes FROM pg_stat_database WHERE datname = current_database()
	`).Scan(&tempFilesBefore, &tempBytesBefore); err != nil {
		t.Fatal(err)
	}

	planRows, err := pool.Query(ctx, `
		EXPLAIN (ANALYZE, BUFFERS, WAL, FORMAT TEXT)
		WITH eligible AS (
			SELECT id FROM telemetry_outbox
			 WHERE written_at < now() - interval '24 hours'
			   AND stream_kind = 'event' AND published_at IS NOT NULL
			 ORDER BY written_at ASC, id ASC
			 LIMIT 2500
			 FOR UPDATE SKIP LOCKED
		)
		DELETE FROM telemetry_outbox USING eligible
		 WHERE telemetry_outbox.id = eligible.id
	`)
	if err != nil {
		t.Fatal(err)
	}
	var planLines []string
	for planRows.Next() {
		var line string
		if err := planRows.Scan(&line); err != nil {
			planRows.Close()
			t.Fatal(err)
		}
		planLines = append(planLines, line)
	}
	if err := planRows.Err(); err != nil {
		planRows.Close()
		t.Fatal(err)
	}
	planRows.Close()
	plan := strings.Join(planLines, "\n")
	if !strings.Contains(plan, "telemetry_outbox_event_gc") {
		t.Fatalf("GC plan did not use telemetry_outbox_event_gc:\n%s", plan)
	}
	t.Logf("GC explain:\n%s", plan)

	durations := make([]time.Duration, 0, 40)
	walBytes := make([]int64, 0, 40)
	totalDeleted := int64(2500)
	for totalDeleted < 100000 {
		var walBefore string
		if err := pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&walBefore); err != nil {
			t.Fatal(err)
		}
		statementStart := time.Now()
		deleted, err := queries.PruneTelemetryOutboxWritten(ctx, db.PruneTelemetryOutboxWrittenParams{
			RetainFor: pgvalue.Interval(24 * time.Hour), RowLimit: 2500,
		})
		durations = append(durations, time.Since(statementStart))
		if err != nil {
			t.Fatal(err)
		}
		if deleted != 2500 {
			t.Fatalf("deleted = %d, want 2500", deleted)
		}
		totalDeleted += deleted
		var walAfter string
		var wal int64
		if err := pool.QueryRow(ctx, `
			SELECT pg_current_wal_insert_lsn()::text,
			       pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint
		`, walBefore).Scan(&walAfter, &wal); err != nil {
			t.Fatal(err)
		}
		walBytes = append(walBytes, wal)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	sort.Slice(walBytes, func(i, j int) bool { return walBytes[i] < walBytes[j] })
	p95 := durations[(len(durations)*95+99)/100-1]
	maxWAL := walBytes[len(walBytes)-1]
	var tempFilesAfter, tempBytesAfter int64
	if err := pool.QueryRow(ctx, `
		SELECT temp_files, temp_bytes FROM pg_stat_database WHERE datname = current_database()
	`).Scan(&tempFilesAfter, &tempBytesAfter); err != nil {
		t.Fatal(err)
	}
	var eligible int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM telemetry_outbox
		 WHERE written_at < now() - interval '24 hours'
		   AND stream_kind = 'event' AND published_at IS NOT NULL
	`).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	t.Logf("GC budget: p95=%s max_wal=%d temp_files=%d temp_bytes=%d table_bytes=%d index_bytes=%d",
		p95, maxWAL, tempFilesAfter-tempFilesBefore, tempBytesAfter-tempBytesBefore, tableBytes, indexBytes)
	if p95 > 100*time.Millisecond {
		t.Fatalf("GC statement p95 = %s, budget <= 100ms", p95)
	}
	if maxWAL > 8<<20 {
		t.Fatalf("GC max WAL = %d, budget <= %d", maxWAL, 8<<20)
	}
	if tempFilesAfter != tempFilesBefore || tempBytesAfter != tempBytesBefore {
		t.Fatalf("GC created temp files/bytes: files=%d bytes=%d", tempFilesAfter-tempFilesBefore, tempBytesAfter-tempBytesBefore)
	}
	if eligible != 0 {
		t.Fatalf("eligible rows after 40 batches = %d, want 0", eligible)
	}

}
