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
	return telemetry.ParseCursor(value)
}
