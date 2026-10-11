package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (server *Server) workerPreparationLog(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationLogRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	defer clear(request.Executor.ChannelCredential)
	host := workerFromContext(r.Context())
	executor, err := preparationExecutor(request.Executor)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	record := diagnostic.Record{Stream: request.Stream, Kind: request.Kind, Sequence: request.Sequence, ThroughSequence: request.ThroughSequence, ObservedAtUnixNano: request.ObservedAtUnixNano, Data: request.Data, DroppedBytes: request.DroppedBytes, Complete: request.Complete}
	receipt, err := agent.AppendPreparationLog(r.Context(), server.diagnosticDB, host, executor, record, server.diagnosticBounds)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, workerapi.DiagnosticLogReceipt{Expired: receipt.Expired, ThroughSequence: receipt.ThroughSequence, AcceptedAt: receipt.AcceptedAt, ExpiresAt: receipt.ExpiresAt})
	case errors.Is(err, telemetry.ErrDiagnosticInvalid):
		writeError(w, badRequest(err))
	case telemetry.IsDiagnosticBusy(err):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "diagnostic_busy"})
	case errors.Is(err, telemetry.ErrDiagnosticCapacity):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "diagnostic_capacity_unavailable"})
	case errors.Is(err, telemetry.ErrDiagnosticClosed):
		writeJSON(w, http.StatusGone, map[string]string{"error": "diagnostic_stream_closed"})
	case errors.Is(err, telemetry.ErrDiagnosticStale):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "diagnostic_retry_superseded"})
	case errors.Is(err, telemetry.ErrDiagnosticConflict), errors.Is(err, telemetry.ErrDiagnosticSequence):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "diagnostic_identity_conflict"})
	default:
		server.writeAllocationError(w, err)
	}
}
