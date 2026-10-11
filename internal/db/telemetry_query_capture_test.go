package db_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var errTelemetryQueryCaptured = errors.New("telemetry production query captured")

type telemetryQueryCapture struct {
	query string
	args  []any
}

func (capture *telemetryQueryCapture) record(query string, args []any) {
	capture.query = query
	capture.args = append([]any(nil), args...)
}

func (capture *telemetryQueryCapture) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	capture.record(query, args)
	return pgconn.CommandTag{}, errTelemetryQueryCaptured
}

func (capture *telemetryQueryCapture) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	capture.record(query, args)
	return nil, errTelemetryQueryCaptured
}

func (capture *telemetryQueryCapture) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	capture.record(query, args)
	return telemetryCapturedRow{}
}

type telemetryCapturedRow struct{}

func (telemetryCapturedRow) Scan(...any) error {
	return errTelemetryQueryCaptured
}
