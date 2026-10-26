package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/telemetry"
)

var (
	errTelemetryInvalidCursor = codedError{code: "invalid_cursor", message: "cursor is invalid"}
	errTelemetryLagging       = codedError{code: "telemetry_lagging", message: "telemetry replay is lagging"}
	errTelemetryUnavailable   = codedError{code: "telemetry_unavailable", message: "telemetry historical store is unavailable"}
)

func writeTelemetryError(w http.ResponseWriter, err error) {
	if _, ok := errors.AsType[apiError](err); ok {
		writeError(w, err)
		return
	}
	if _, ok := errors.AsType[telemetry.LaggingError](err); ok {
		writeError(w, unavailable(errTelemetryLagging))
		return
	}
	if errors.Is(err, telemetry.ErrHistoricalUnavailable) {
		writeError(w, unavailable(errTelemetryUnavailable))
		return
	}
	writeError(w, err)
}
