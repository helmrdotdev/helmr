package control

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/telemetry"
)

var (
	errTelemetryInvalidCursor = codedError{code: "invalid_cursor", message: "cursor is invalid"}
	errTelemetryLagging       = codedError{code: "telemetry_lagging", message: "telemetry replay is lagging"}
	errTelemetryUnavailable   = codedError{code: "telemetry_unavailable", message: "telemetry historical store is unavailable"}
	errRunCellScopeDenied     = codedError{code: "cell_scope_denied", message: "resource is not available in this control cell"}
)

func (s *Server) rejectRunFromWrongCell(w http.ResponseWriter, runCellID string) bool {
	if runCellID == s.cellID {
		return false
	}
	writeError(w, forbidden(errRunCellScopeDenied))
	return true
}

func writeRunTelemetryError(w http.ResponseWriter, err error) {
	var apiErr apiError
	if errors.As(err, &apiErr) {
		writeError(w, err)
		return
	}
	var lagging telemetry.LaggingError
	if errors.As(err, &lagging) {
		writeError(w, unavailable(errTelemetryLagging))
		return
	}
	if errors.Is(err, telemetry.ErrHistoricalUnavailable) {
		writeError(w, unavailable(errTelemetryUnavailable))
		return
	}
	writeError(w, err)
}
