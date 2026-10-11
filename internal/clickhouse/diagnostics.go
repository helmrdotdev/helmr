package clickhouse

import (
	"context"
	"fmt"
	"math"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/helmrdotdev/helmr/internal/telemetry"
)

// Fixed owners select separate histories. This mapping is never an ACL or an
// SQL identifier supplied by the caller.
func diagnosticTable(kind string) (string, string, error) {
	switch kind {
	case "session":
		return "session_logs", "session_id", nil
	case "computer_preparation":
		return "computer_preparation_logs", "preparation_id", nil
	case "computer_command":
		return "computer_command_logs", "command_id", nil
	default:
		return "", "", telemetry.ErrDiagnosticInvalid
	}
}

// WriteDiagnostics confirms synchronous sink acceptance. Raw bytes use the
// native String column encoding; no JSON or UTF-8 conversion changes their bytes.
// Row-local encoding failures are isolated without losing healthy neighbors.
func (w *Writer) WriteDiagnostics(ctx context.Context, kind string, rows []telemetry.StoredDiagnostic) ([]telemetry.RejectedRow, error) {
	table, owner, err := diagnosticTable(kind)
	if err != nil {
		return nil, err
	}
	columns := []ch.ColumnNameAndType{
		{Name: "environment_id", Type: "UUID"}, {Name: owner, Type: "UUID"},
		{Name: "producer_epoch", Type: "Int64"}, {Name: "stream", Type: "Enum8('stdout'=1,'stderr'=2)"},
		{Name: "sequence", Type: "Int64"}, {Name: "through_sequence", Type: "Int64"},
		{Name: "kind", Type: "Enum8('data'=1,'gap'=2,'end'=3)"}, {Name: "observed_at_unix_nano", Type: "Int64"},
		{Name: "data", Type: "String"}, {Name: "dropped_bytes", Type: "Int64"}, {Name: "complete", Type: "Bool"},
		{Name: "byte_offset", Type: "Int64"}, {Name: "through_byte_offset", Type: "Int64"},
		{Name: "accepted_at", Type: "DateTime64(6, 'UTC')"}, {Name: "expires_at", Type: "DateTime64(6, 'UTC')"},
	}
	valid := make([]int, 0, len(rows))
	var rejected []telemetry.RejectedRow
	for i, row := range rows {
		if row.Source.Kind != kind || row.Source.Validate() != nil || row.Record.Validate(math.MaxInt64) != nil || row.AcceptedAt.IsZero() || row.ExpiresAt.Sub(row.AcceptedAt) != 90*24*time.Hour || row.AcceptedAt.Nanosecond()%1000 != 0 || row.ByteOffset < 0 || row.ThroughByteOffset < row.ByteOffset || row.ThroughByteOffset-row.ByteOffset != int64(len(row.Record.Data))+row.Record.DroppedBytes {
			rejected = append(rejected, telemetry.RejectedRow{Index: i, Err: telemetry.ErrDiagnosticInvalid})
			continue
		}
		valid = append(valid, i)
	}
	for len(valid) > 0 {
		batch, err := w.client.PrepareBatch(ch.Context(ctx, ch.WithSettings(ch.Settings{"async_insert": 0}), ch.WithColumnNamesAndTypes(columns)), fmt.Sprintf(`INSERT INTO helmr_telemetry.%s(environment_id,%s,producer_epoch,stream,sequence,through_sequence,kind,observed_at_unix_nano,data,dropped_bytes,complete,byte_offset,through_byte_offset,accepted_at,expires_at)`, table, owner))
		if err != nil {
			return rejected, err
		}
		rejectedAt := -1
		for position, index := range valid {
			row := rows[index]
			record := row.Record
			if err = batch.Append(row.Source.EnvironmentID, row.Source.ID, row.Source.ProducerEpoch, record.Stream, record.Sequence, record.ThroughSequence, record.Kind, record.ObservedAtUnixNano, record.Data, record.DroppedBytes, record.Complete, row.ByteOffset, row.ThroughByteOffset, row.AcceptedAt, row.ExpiresAt); err != nil {
				rejected = append(rejected, telemetry.RejectedRow{Index: index, Err: fmt.Errorf("encode diagnostic row %d: %w", index, err)})
				rejectedAt = position
				break
			}
		}
		if rejectedAt >= 0 {
			_ = batch.Close()
			valid = append(valid[:rejectedAt], valid[rejectedAt+1:]...)
			continue
		}
		err = batch.Send()
		_ = batch.Close()
		return rejected, err
	}
	return rejected, nil
}
