package db_test

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

func TestStoredEventIngestSizeTracksNormalizedOctetsAndClaimBoundary(t *testing.T) {
	ctx := t.Context()
	pool := newPostgresDB(t, ctx)
	var id, size int64
	err := pool.QueryRow(ctx, `INSERT INTO telemetry_outbox(org_id,stream_kind,source_kind,source_id,project_id,environment_id,run_id,kind,message,payload) VALUES($1,'event','run',$2,$3,$4,$2,'test','進行','{"v":1}') RETURNING id,ingest_size_bytes`, uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()).Scan(&id, &size)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(`進行{"v": 1}`)) {
		t.Fatalf("normalized UTF-8 bytes: %d", size)
	}
	q := db.New(pool)
	params := db.ClaimEventIngestBatchParams{RowLimit: 10000, MaxBatchBytes: size - 1, LeaseDuration: pgvalue.Interval(30 * time.Second)}
	if rows, err := q.ClaimEventIngestBatch(ctx, params); err != nil || len(rows) != 0 {
		t.Fatalf("over-budget event claimed: %d %v", len(rows), err)
	}
	params.MaxBatchBytes = size
	if rows, err := q.ClaimEventIngestBatch(ctx, params); err != nil || len(rows) != 1 || rows[0].OutboxID != id {
		t.Fatalf("exact byte boundary: %v %v", rows, err)
	}
	err = pool.QueryRow(ctx, `UPDATE telemetry_outbox SET message='x',payload='{"v":"é"}' WHERE id=$1 RETURNING ingest_size_bytes`, id).Scan(&size)
	if err != nil || size != int64(len(`x{"v": "é"}`)) {
		t.Fatalf("updated normalized bytes: %d %v", size, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE telemetry_outbox SET ingest_size_bytes=1 WHERE id=$1`, id); err == nil {
		t.Fatal("generated size was manually overridden")
	}
}

func TestStoredLogIngestSizeUsesAdmittedSize(t *testing.T) {
	f := runtest.New(t)
	w := newTelemetryRunLease(t, f)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO telemetry_outbox(org_id,stream_kind,source_kind,source_id,project_id,environment_id,run_id,run_lease_id,attempt_number,stream_name,content,size_bytes,observed_seq) VALUES($1,'run_log','run',$2,$3,$4,$2,$5,1,'stdout',convert_to('hello','UTF8'),5,1)`, f.OrgID, w.RunID, f.ProjectID, f.EnvironmentID, w.LeaseID)
	var size int64
	if err := f.Pool.QueryRow(ctx, `SELECT ingest_size_bytes FROM telemetry_outbox WHERE run_lease_id=$1 AND stream_kind='run_log'`, w.LeaseID).Scan(&size); err != nil || size != 5 {
		t.Fatalf("log size: %d %v", size, err)
	}
	if err := f.Pool.QueryRow(ctx, `UPDATE telemetry_outbox SET content=convert_to('world!','UTF8'),size_bytes=6 WHERE run_lease_id=$1 AND stream_kind='run_log' RETURNING ingest_size_bytes`, w.LeaseID).Scan(&size); err != nil || size != 6 {
		t.Fatalf("updated log size: %d %v", size, err)
	}
}
