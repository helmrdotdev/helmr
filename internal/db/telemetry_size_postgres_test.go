package db_test

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestStoredEventIngestSizeTracksNormalizedOctetsAndClaimBoundary(t *testing.T) {
	ctx := t.Context()
	f := agenttest.New(t)
	pool := f.Pool
	var id, size int64
	err := pool.QueryRow(ctx, `INSERT INTO telemetry_outbox(environment_id,deployment_id,stream_kind,kind,message,payload) VALUES($1,$2,'event','test','進行','{"v":1}') RETURNING id,ingest_size_bytes`, f.Environment, f.Deployment).Scan(&id, &size)
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

func TestStoredDiagnosticIngestSizeTracksData(t *testing.T) {
	f := agenttest.New(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO telemetry_outbox(environment_id,session_id,process_epoch,stream_kind,stream,sequence,through_sequence,byte_offset,through_byte_offset,kind,observed_at_unix_nano,data,dropped_bytes,complete,accepted_at,expires_at) VALUES($1,$2,1,'diagnostic','stdout',1,1,0,5,'data',1,convert_to('hello','UTF8'),0,false,now(),now()+interval '2160 hours')`, f.Environment, f.Session)
	var size int64
	if err := f.Pool.QueryRow(ctx, `SELECT ingest_size_bytes FROM telemetry_outbox WHERE session_id=$1`, f.Session).Scan(&size); err != nil || size != 5 {
		t.Fatalf("diagnostic size=%d %v", size, err)
	}
	if err := f.Pool.QueryRow(ctx, `UPDATE telemetry_outbox SET data=convert_to('world!','UTF8'),through_byte_offset=6 WHERE session_id=$1 RETURNING ingest_size_bytes`, f.Session).Scan(&size); err != nil || size != 6 {
		t.Fatalf("updated diagnostic size=%d %v", size, err)
	}
}
