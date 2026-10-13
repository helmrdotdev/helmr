package db_test

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/clickhouse"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
)

// Opt-in memory/lease measurement: keep actual claimed PostgreSQL payloads alive
// through a Native/LZ4 send. Run each case in a fresh process when measuring RSS.
func TestTelemetryClaimEnvelopeMeasurement(t *testing.T) {
	if os.Getenv("HELMR_TEST_TELEMETRY_ENVELOPE") != "1" {
		t.Skip("HELMR_TEST_TELEMETRY_ENVELOPE is not set")
	}
	for _, kind := range []string{"logs"} {
		for _, mib := range []int{8, 16, 32} {
			t.Run(fmt.Sprintf("%s/mib%d", kind, mib), func(t *testing.T) {
				f := agenttest.New(t)
				client, err := clickhouse.New(clickhouse.Config{URL: os.Getenv("HELMR_TEST_CLICKHOUSE_URL")})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				writer := clickhouse.NewWriter(client)
				ctx := ch.Context(t.Context(), ch.WithSettings(ch.Settings{"async_insert": 0}))

				dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO diagnostic_chunks(environment_id,session_id,process_epoch,stream,sequence,through_sequence,byte_offset,through_byte_offset,kind,observed_at_unix_nano,data,dropped_bytes,complete,accepted_at,expires_at) SELECT $1,$2,1,'diagnostic','stdout',n,n,(n-1)*196608,n*196608,'data',1,convert_to(repeat('x',196608),'UTF8'),0,false,now(),now()+interval '2160 hours' FROM generate_series(1,256)n`, f.Environment, f.Session)
				start := time.Now()
				var claimTime time.Duration
				var count, total int
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

				elapsed := time.Since(start)
				t.Logf("rows=%d payload_bytes=%d claim=%s claim_and_send=%s", count, total, claimTime, elapsed)
				if count == 0 || total > mib<<20 || elapsed > 25*time.Second {
					t.Fatal("claim envelope or lease budget exceeded")
				}
			})
		}
	}
}
