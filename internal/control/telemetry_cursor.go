package control

import (
	"strings"

	"github.com/helmrdotdev/helmr/internal/telemetry"
)

func telemetryCursor(seq int64) string {
	return telemetry.Cursor(seq)
}

func parseTelemetryCursor(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	seq, err := telemetry.ParseCursor(value)
	if err != nil {
		return 0, errTelemetryInvalidCursor
	}
	return seq, nil
}
