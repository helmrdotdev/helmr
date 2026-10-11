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
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
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
				f := agenttest.New(t)
				client, err := clickhouse.New(clickhouse.Config{URL: os.Getenv("HELMR_TEST_CLICKHOUSE_URL")})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				writer := clickhouse.NewWriter(client)
				q := db.New(f.Pool)
				ctx := ch.Context(t.Context(), ch.WithSettings(ch.Settings{"async_insert": 0}))

				if kind == "logs" {
					dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO telemetry_outbox(environment_id,session_id,process_epoch,stream_kind,stream,sequence,through_sequence,byte_offset,through_byte_offset,kind,observed_at_unix_nano,data,dropped_bytes,complete,accepted_at,expires_at) SELECT $1,$2,1,'diagnostic','stdout',n,n,(n-1)*196608,n*196608,'data',1,convert_to(repeat('x',196608),'UTF8'),0,false,now(),now()+interval '2160 hours' FROM generate_series(1,256)n`, f.Environment, f.Session)
				} else {
					payload := make([]byte, 49149)
					if os.Getenv("HELMR_TEST_TELEMETRY_ENTROPY") == "random" {
						_, _ = rand.NewChaCha8([32]byte{7}).Read(payload)
					}
					dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO telemetry_outbox(environment_id,deployment_id,stream_kind,kind,message,payload) SELECT $1,$2,'event','deployment.test',repeat('m',4096),to_jsonb($3::text) FROM generate_series(1,600)n`, f.Environment, f.Deployment, base64.StdEncoding.EncodeToString(payload))
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

					claims, err := telemetry.ClaimDiagnostics(ctx, f.Pool, "session", diagnostic.ExportBounds{Records: 10000, Bytes: int64(mib << 20), ClaimFor: 30 * time.Second})
					if err != nil {
						t.Fatal(err)
					}
					claimTime = time.Since(start)
					count = len(claims.Records)
					for _, r := range claims.Records {
						total += len(r.Record.Data)
					}
					if reject, err := writer.WriteDiagnostics(ctx, "session", claims.Records); err != nil || len(reject) > 0 {
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
						deployment := pgvalue.MustUUIDValue(r.DeploymentID)
						records[i] = telemetry.EventRecord{OrgID: pgvalue.MustUUIDValue(r.OrgID), ProjectID: pgvalue.MustUUIDValue(r.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(r.EnvironmentID), SubjectKind: r.SubjectType, SubjectID: pgvalue.MustUUIDValue(r.SubjectID), DeploymentID: &deployment, EventKind: r.Kind, Seq: uint64(r.Seq), Message: r.Message, Body: string(r.Payload), ObservedAt: pgvalue.Time(r.CreatedAt), AcceptedAt: pgvalue.Time(r.CreatedAt)}
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
