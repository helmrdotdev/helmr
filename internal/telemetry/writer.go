package telemetry

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/clickhouse"
)

type ClickHouseWriter struct {
	client *clickhouse.Client
}

func NewClickHouseWriter(client *clickhouse.Client) *ClickHouseWriter {
	return &ClickHouseWriter{client: client}
}

func (w *ClickHouseWriter) WriteEvents(ctx context.Context, rows []EventRecord) error {
	if len(rows) == 0 {
		return nil
	}
	return w.client.InsertJSONEachRow(ctx, `INSERT INTO helmr_telemetry.events FORMAT JSONEachRow`, rows)
}

func (w *ClickHouseWriter) WriteRunLogs(ctx context.Context, rows []RunLogRecord) error {
	if len(rows) == 0 {
		return nil
	}
	return w.client.InsertJSONEachRow(ctx, `INSERT INTO helmr_telemetry.run_logs FORMAT JSONEachRow`, rows)
}

func (w *ClickHouseWriter) WriteTerminalOutput(ctx context.Context, rows []TerminalOutputRecord) error {
	if len(rows) == 0 {
		return nil
	}
	return w.client.InsertJSONEachRow(ctx, `INSERT INTO helmr_telemetry.terminal_output FORMAT JSONEachRow`, rows)
}
