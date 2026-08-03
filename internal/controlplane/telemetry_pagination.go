package controlplane

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/telemetry"
)

const (
	runEventsPageSize          = int32(200)
	runEventsFollowMaxDuration = 30 * time.Minute
)

func eventCursor(r *http.Request) (int64, error) {
	value := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if value == "" {
		value = strings.TrimSpace(r.URL.Query().Get("cursor"))
	}
	if value == "" {
		return 0, nil
	}
	seq, err := telemetry.ParseCursor(value)
	if err != nil {
		return 0, errTelemetryInvalidCursor
	}
	return seq, nil
}

func eventLimit(r *http.Request) (int32, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return runEventsPageSize, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 1 || parsed > int64(runEventsPageSize) {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d", runEventsPageSize)
	}
	return int32(parsed), nil
}
