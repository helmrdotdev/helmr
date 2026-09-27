package telemetry

import (
	"bytes"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"testing"
	"time"
)

func TestCommandLogDeliveryKeepsScopeAndAcknowledgesOnlyWrittenRows(t *testing.T) {
	accepted := time.Now().UTC().Truncate(time.Millisecond)
	row := db.ClaimCommandLogIngestBatchRow{OutboxID: 7, RetryCount: 2, OrgID: pgvalue.NewUUIDv7(), ProjectID: pgvalue.NewUUIDv7(), EnvironmentID: pgvalue.NewUUIDv7(), CommandID: pgvalue.NewUUIDv7(), Stream: "stderr", ObservedSeq: pgtype.Int8{Int64: 0, Valid: true}, Content: []byte{0, 255, 128}, SizeBytes: pgtype.Int8{Int64: 3, Valid: true}, CreatedAt: pgtype.Timestamptz{Time: accepted, Valid: true}, ObservedAt: pgtype.Timestamptz{Time: accepted.Add(-time.Second), Valid: true}}
	second := row
	second.OutboxID = 8
	second.ObservedSeq.Int64 = 1
	for _, mode := range []string{"success", "sink unavailable", "row rejected"} {
		t.Run(mode, func(t *testing.T) {
			store := &fakeIngestStore{commandLogRows: []db.ClaimCommandLogIngestBatchRow{row, second}}
			writer := &fakeIngestWriter{}
			if mode == "sink unavailable" {
				writer.commandLogErr = errors.New("offline")
			}
			if mode == "row rejected" {
				writer.commandLogResult = []RejectedRow{{Index: 1, Err: errors.New("bad row")}}
			}
			ingester := testIngestor(store, writer)
			ingester.batchBytes = MaxTelemetryBatchBytes
			count, err := ingester.ingestCommandLogs(t.Context())
			if count != 2 || (err != nil) != (mode != "success") {
				t.Fatalf("ingest=%d, %v", count, err)
			}
			got := writer.commandLogRows[0]
			if writer.commandLogCalls != 1 || got.CommandID != pgvalue.MustUUIDValue(row.CommandID) || !bytes.Equal(got.Content, row.Content) || !got.AcceptedAt.Equal(accepted) || !got.ObservedAt.Equal(row.ObservedAt.Time) {
				t.Fatalf("delivery=%+v", got)
			}
			wantWritten, wantFailed := 2, 0
			if mode == "sink unavailable" {
				wantWritten, wantFailed = 0, 2
			}
			if mode == "row rejected" {
				wantWritten, wantFailed = 1, 1
			}
			if len(store.writtenIDs) != wantWritten || len(store.failedIDs) != wantFailed {
				t.Fatalf("written=%v failed=%v", store.writtenIDs, store.failedIDs)
			}
			if store.commandLogClaims[0].MaxBatchBytes != MaxTelemetryBatchBytes {
				t.Fatal("missing byte budget")
			}
		})
	}
}
