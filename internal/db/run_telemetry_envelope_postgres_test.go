package db_test

import (
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/telemetry"
)

// Opt-in memory/lease measurement: keep actual claimed PostgreSQL payloads alive
// through a Native/LZ4 send. Run each case in a fresh process when measuring RSS.
func TestTelemetryClaimEnvelopeMeasurement(t *testing.T) {
	if os.Getenv("HELMR_TEST_TELEMETRY_ENVELOPE") != "1" {
		t.Skip("HELMR_TEST_TELEMETRY_ENVELOPE is not set")
	}
	for _, kind := range []string{"events", "logs"} {
		for _, mib := range []int{8, 16, 32} {
			t.Run(fmt.Sprintf("%s/mib%d", kind, mib), func(t *testing.T) {
				f := runtest.New(t)
				work := newTelemetryRunLease(t, f)
				client, err := clickhouse.New(clickhouse.Config{URL: os.Getenv("HELMR_TEST_CLICKHOUSE_URL")})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				writer := clickhouse.NewWriter(client)
				q := db.New(f.Pool)
				ctx := ch.Context(t.Context(), ch.WithSettings(ch.Settings{"async_insert": 0}))
				base := `INSERT INTO telemetry_outbox(org_id,stream_kind,source_kind,source_id,project_id,environment_id,run_id,run_lease_id,attempt_number,stream_name,content,size_bytes,observed_seq,kind,message,payload) SELECT $1,`
				if kind == "logs" {
					dbtest.MustExec(t, ctx, f.Pool, base+`'run_log','run',$2,$3,$4,$2,$5,1,'stdout',convert_to(repeat('x',196608),'UTF8'),196608,n,'run.log','','{}'::jsonb FROM generate_series(1,10000)n`, f.OrgID, work.RunID, f.ProjectID, f.EnvironmentID, work.LeaseID)
				} else {
					payload := make([]byte, 49149)
					if os.Getenv("HELMR_TEST_TELEMETRY_ENTROPY") == "random" {
						_, _ = rand.NewChaCha8([32]byte{7}).Read(payload)
					}
					dbtest.MustExec(t, ctx, f.Pool, base+`'event','run',$2,$3,$4,$2,$5,1,'',NULL,NULL,NULL,'run.test',repeat('m',4096),to_jsonb($6::text) FROM generate_series(1,10000)n`, f.OrgID, work.RunID, f.ProjectID, f.EnvironmentID, work.LeaseID, base64.StdEncoding.EncodeToString(payload))
					if os.Getenv("HELMR_TEST_TELEMETRY_ENTROPY") == "random" {
						capture := &telemetryQueryCapture{}
						_, _ = db.New(capture).ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{RowLimit: 10000, MaxBatchBytes: int64(mib << 20), LeaseDuration: pgvalue.Interval(30 * time.Second)})
						tx, err := f.Pool.Begin(ctx)
						if err != nil {
							t.Fatal(err)
						}
						var plan string
						err = tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+capture.query, capture.args...).Scan(&plan)
						_ = tx.Rollback(ctx)
						if err != nil {
							t.Fatal(err)
						}
						t.Logf("random candidate scan plan=%s", plan)
					}
				}
				start := time.Now()
				var claimTime time.Duration
				var count, total int
				if kind == "logs" {
					claims, err := q.ClaimRunLogIngestBatch(ctx, db.ClaimRunLogIngestBatchParams{RowLimit: 10000, MaxBatchBytes: int64(mib << 20), LeaseDuration: pgvalue.Interval(30 * time.Second)})
					if err != nil {
						t.Fatal(err)
					}
					claimTime = time.Since(start)
					records := make([]telemetry.RunLogRecord, len(claims))
					for i, r := range claims {
						records[i] = telemetry.RunLogRecord{OrgID: pgvalue.MustUUIDValue(r.OrgID), ProjectID: pgvalue.MustUUIDValue(r.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(r.EnvironmentID), RunID: pgvalue.MustUUIDValue(r.RunID), RunLeaseID: pgvalue.MustUUIDValue(r.RunLeaseID), AttemptNumber: r.AttemptNumber.Int32, StreamName: string(r.Stream), Seq: uint64(r.Seq), ObservedSeq: uint64(r.ObservedSeq.Int64), Content: r.Content, SizeBytes: uint32(r.SizeBytes.Int64), ObservedAt: pgvalue.Time(r.CreatedAt), AcceptedAt: pgvalue.Time(r.CreatedAt)}
						total += len(r.Content)
					}
					count = len(records)
					if reject, err := writer.WriteRunLogs(ctx, records); err != nil || len(reject) > 0 {
						t.Fatalf("write: %v %v", reject, err)
					}
					runtime.KeepAlive(claims)
				} else {
					claims, err := q.ClaimEventIngestBatch(ctx, db.ClaimEventIngestBatchParams{RowLimit: 10000, MaxBatchBytes: int64(mib << 20), LeaseDuration: pgvalue.Interval(30 * time.Second)})
					if err != nil {
						t.Fatal(err)
					}
					claimTime = time.Since(start)
					records := make([]telemetry.EventRecord, len(claims))
					for i, r := range claims {
						run := pgvalue.MustUUIDValue(r.RunID)
						records[i] = telemetry.EventRecord{OrgID: pgvalue.MustUUIDValue(r.OrgID), ProjectID: pgvalue.MustUUIDValue(r.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(r.EnvironmentID), SubjectKind: r.SubjectType, SubjectID: pgvalue.MustUUIDValue(r.SubjectID), RunID: &run, EventKind: r.Kind, Seq: uint64(r.Seq), Message: r.Message, Body: string(r.Payload), ObservedAt: pgvalue.Time(r.CreatedAt), AcceptedAt: pgvalue.Time(r.CreatedAt)}
						total += len(r.Message) + len(r.Payload)
					}
					count = len(records)
					if reject, err := writer.WriteEvents(ctx, records); err != nil || len(reject) > 0 {
						t.Fatalf("write: %v %v", reject, err)
					}
					runtime.KeepAlive(claims)
				}
				elapsed := time.Since(start)
				t.Logf("rows=%d payload_bytes=%d claim=%s claim_and_send=%s", count, total, claimTime, elapsed)
				if count == 0 || total > mib<<20 || elapsed > 25*time.Second {
					t.Fatal("claim envelope or lease budget exceeded")
				}
			})
		}
	}
}
