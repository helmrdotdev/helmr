package clickhouse

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/jackc/pgx/v5"
)

func TestDiagnosticHistoriesPreserveBinaryIdentityAndOffsets(t *testing.T) {
	client := disposableClient(t)
	writer := NewWriter(client)
	for _, kind := range []string{"session", "computer_preparation", "computer_command"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Microsecond)
			source := telemetry.DiagnosticSource{EnvironmentID: uuid.New(), Kind: kind, ID: uuid.New(), ProducerEpoch: 7}
			raw := []byte{0, 255, 128, 'A', '\n'}
			records := []telemetry.StoredDiagnostic{
				{Source: source, Record: diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: now.UnixNano() + 123, Data: raw}, ThroughByteOffset: 5, AcceptedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)},
				{Source: source, Record: diagnostic.Record{Stream: "stdout", Kind: "end", Sequence: 2, ThroughSequence: 2, ObservedAtUnixNano: now.UnixNano() + 456, Complete: true}, ByteOffset: 5, ThroughByteOffset: 5, AcceptedAt: now.Add(time.Microsecond), ExpiresAt: now.Add(time.Microsecond + 90*24*time.Hour)},
			}
			poison := records[0]
			poison.Record.Sequence = 0
			for range 2 {
				rejected, err := writer.WriteDiagnostics(t.Context(), kind, append(records, poison))
				if err != nil || len(rejected) != 1 || rejected[0].Index != 2 {
					t.Fatalf("write %v %v", rejected, err)
				}
			}
			table, owner, err := diagnosticTable(kind)
			if err != nil {
				t.Fatal(err)
			}
			var rows []struct {
				Sequence      int64     `ch:"sequence"`
				Data          string    `ch:"data"`
				Observed      int64     `ch:"observed_at_unix_nano"`
				Offset        int64     `ch:"byte_offset"`
				ThroughOffset int64     `ch:"through_byte_offset"`
				Accepted      time.Time `ch:"accepted_at"`
				Expires       time.Time `ch:"expires_at"`
				Complete      bool      `ch:"complete"`
			}
			err = client.Select(t.Context(), &rows, fmt.Sprintf(`SELECT sequence,data,observed_at_unix_nano,byte_offset,through_byte_offset,accepted_at,expires_at,complete FROM helmr_telemetry.%s FINAL WHERE environment_id=? AND %s=? AND producer_epoch=? AND stream='stdout' ORDER BY sequence LIMIT 3 SETTINGS max_rows_to_read=1000,max_bytes_to_read=1000000,max_memory_usage=100000000,max_execution_time=5`, table, owner), source.EnvironmentID, source.ID, source.ProducerEpoch)
			if err != nil || len(rows) != 2 {
				t.Fatalf("deduplicated history %+v %v", rows, err)
			}
			if !bytes.Equal([]byte(rows[0].Data), raw) || rows[0].Observed != records[0].Record.ObservedAtUnixNano || (rows[0].Offset != 0 || rows[0].ThroughOffset != 5) || !rows[0].Accepted.Equal(now) || !rows[0].Expires.Equal(records[0].ExpiresAt) {
				t.Fatalf("raw envelope changed %+v", rows[0])
			}
			if !rows[1].Complete || (rows[1].Offset != 5 || rows[1].ThroughOffset != 5) || !rows[1].Accepted.Equal(now.Add(time.Microsecond)) {
				t.Fatalf("byte offset changed %+v", rows[1])
			}
		})
	}
}

func TestDiagnosticHistoriesFromPostgresAcceptance(t *testing.T) {
	client := disposableClient(t)
	writer := NewWriter(client)
	bounds := diagnostic.Bounds{ChunkBytes: 5, SourceBytes: 10, SourceRecords: 4, EnvironmentBytes: 20, EnvironmentRecords: 8, QueueBytes: 40, QueueRecords: 16}
	for _, kind := range []string{"session", "computer_preparation", "computer_command"} {
		t.Run(kind, func(t *testing.T) {
			f := agenttest.New(t)
			source := telemetry.DiagnosticSource{EnvironmentID: f.Environment, Kind: kind, ID: f.Session, ProducerEpoch: 1}
			switch kind {
			case "computer_preparation":
				source.ID = uuid.NewV7()
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_preparations(environment_id,id,preparation_spec_id,retry_key,status,deadline_at,executor_epoch,worker_host_id,worker_epoch,instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest)
              SELECT $1,$2,$3,'diagnostics','running',clock_timestamp()+interval '1 hour',1,worker_host_id,worker_epoch,$2,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest FROM computer_leases LIMIT 1`, f.Environment, source.ID, f.Deployment)
			case "computer_command":
				source.ID = uuid.NewV7()
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(environment_id,id,computer_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,ARRAY['true'],'{}','',1000,'user','test')`, f.Environment, source.ID, f.Computer)
			}
			raw := []byte{0, 255, 128, 'A', '\n'}
			head := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 123456789, Data: raw}
			end := diagnostic.Record{Stream: "stdout", Kind: "end", Sequence: 2, ThroughSequence: 2, ObservedAtUnixNano: 123456790, Complete: true}
			var receipts []telemetry.DiagnosticReceipt
			for _, record := range []diagnostic.Record{head, end} {
				err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
					admission, e := telemetry.BeginDiagnosticAdmission(t.Context(), tx, source, bounds)
					if e != nil {
						return e
					}
					r, e := admission.Append(t.Context(), record)
					receipts = append(receipts, r)
					return e
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			batch := diagnostic.ExportBounds{Records: 4, Bytes: 10, ClaimFor: time.Minute}
			claim, err := telemetry.ClaimDiagnostics(t.Context(), f.Pool, kind, batch)
			if err != nil || len(claim.IDs) != 2 {
				t.Fatalf("claim %+v %v", claim, err)
			}
			// Repeat the sink write to model acceptance whose response was lost.
			for range 2 {
				rejected, e := writer.WriteDiagnostics(t.Context(), kind, claim.Records)
				if e != nil || len(rejected) != 0 {
					t.Fatalf("sink %v %v", rejected, e)
				}
			}
			retired, err := telemetry.RetireDiagnostics(t.Context(), f.Pool, claim.Token, claim.IDs)
			if err != nil || retired.Delivered != 2 || retired.Expired != 0 {
				t.Fatalf("retired %+v %v", retired, err)
			}
			table, owner, err := diagnosticTable(kind)
			if err != nil {
				t.Fatal(err)
			}
			var rows []struct {
				Sequence      int64     `ch:"sequence"`
				Data          string    `ch:"data"`
				Offset        int64     `ch:"byte_offset"`
				ThroughOffset int64     `ch:"through_byte_offset"`
				Accepted      time.Time `ch:"accepted_at"`
				Expires       time.Time `ch:"expires_at"`
				Observed      int64     `ch:"observed_at_unix_nano"`
				Complete      bool      `ch:"complete"`
			}
			err = client.Select(t.Context(), &rows, fmt.Sprintf(`SELECT sequence,data,byte_offset,through_byte_offset,accepted_at,expires_at,observed_at_unix_nano,complete FROM helmr_telemetry.%s FINAL WHERE environment_id=? AND %s=? AND producer_epoch=? AND stream='stdout' ORDER BY sequence LIMIT 3 SETTINGS max_rows_to_read=1000,max_bytes_to_read=1000000,max_memory_usage=100000000,max_execution_time=5`, table, owner), source.EnvironmentID, source.ID, source.ProducerEpoch)
			if err != nil || len(rows) != 2 {
				t.Fatalf("history %+v %v", rows, err)
			}
			if !bytes.Equal([]byte(rows[0].Data), raw) || rows[0].Observed != head.ObservedAtUnixNano || (rows[0].Offset != 0 || rows[0].ThroughOffset != 5) || !rows[0].Accepted.Equal(receipts[0].AcceptedAt) || !rows[0].Expires.Equal(receipts[0].ExpiresAt) {
				t.Fatalf("head changed %+v", rows[0])
			}
			if !rows[1].Complete || (rows[1].Offset != 5 || rows[1].ThroughOffset != 5) || !rows[1].Accepted.Equal(receipts[1].AcceptedAt) || !rows[1].Expires.Equal(receipts[1].ExpiresAt) {
				t.Fatalf("end changed %+v", rows[1])
			}
			err = db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
				admission, e := telemetry.BeginDiagnosticAdmission(t.Context(), tx, source, bounds)
				if e != nil {
					return e
				}
				r, e := admission.Append(t.Context(), end)
				if e == nil && r != receipts[1] {
					t.Fatalf("receipt changed %+v", r)
				}
				return e
			})
			if err != nil {
				t.Fatalf("retry after retirement %v", err)
			}
		})
	}
}

func TestDiagnosticReplayKeepsAcceptedPartitionAndRetention(t *testing.T) {
	c := disposableClient(t)
	writer := NewWriter(c)
	accepted := time.Now().UTC().Truncate(time.Microsecond).Add(-48 * time.Hour)
	for _, kind := range []string{"session", "computer_preparation", "computer_command"} {
		t.Run(kind, func(t *testing.T) {
			table, owner, err := diagnosticTable(kind)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(t.Context(), "SYSTEM STOP MERGES helmr_telemetry."+table); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := c.Exec(context.Background(), "SYSTEM START MERGES helmr_telemetry."+table); err != nil {
					t.Error(err)
				}
			})
			source := telemetry.DiagnosticSource{EnvironmentID: uuid.New(), Kind: kind, ID: uuid.New(), ProducerEpoch: 1}
			rows := diagnosticRows(source, 1, 4, accepted)
			if rejected, err := writer.WriteDiagnostics(t.Context(), kind, rows); err != nil || len(rejected) != 0 {
				t.Fatalf("write: %v %v", rejected, err)
			}
			if err := c.Exec(t.Context(), "INSERT INTO helmr_telemetry."+table+" SELECT * EXCEPT (ingested_at), ingested_at + INTERVAL 1 DAY FROM helmr_telemetry."+table+" WHERE environment_id = ? AND "+owner+" = ?", source.EnvironmentID, source.ID); err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(t.Context(), "SELECT throwIf(count() != 2 OR uniqExact(_partition_id) != 1 OR uniqExact(accepted_at) != 1 OR uniqExact(toDate(ingested_at)) != 2 OR min(expires_at) != max(expires_at) OR toUnixTimestamp64Micro(min(expires_at)) != ?, 'unstable diagnostic replay retention') FROM helmr_telemetry."+table+" WHERE environment_id = ? AND "+owner+" = ?", rows[0].ExpiresAt.UnixMicro(), source.EnvironmentID, source.ID); err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(t.Context(), "SELECT throwIf(position(create_table_query, 'TTL expires_at') = 0 OR position(create_table_query, 'PARTITION BY toDate(accepted_at)') = 0, 'unexpected diagnostic retention DDL') FROM system.tables WHERE database = 'helmr_telemetry' AND name = ?", table); err != nil {
				t.Fatal(err)
			}
		})
	}
}
