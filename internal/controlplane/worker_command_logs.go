package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerAppendCommandLogs(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CommandLogAppendRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	chunk, err := commandLogChunk(request)
	var receipt telemetry.DiagnosticReceipt
	if err == nil {
		receipt, err = command.AppendLog(r.Context(), s.diagnosticDB, workerFromContext(r.Context()), chunk, s.diagnosticBounds)
	}
	if telemetry.IsDiagnosticBusy(err) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "diagnostic_busy"})
		return
	}
	if errors.Is(err, telemetry.ErrDiagnosticCapacity) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "diagnostic_capacity_unavailable"})
		return
	}
	if errors.Is(err, telemetry.ErrDiagnosticInvalid) {
		writeError(w, badRequest(err))
		return
	}
	if errors.Is(err, telemetry.ErrDiagnosticClosed) {
		writeJSON(w, http.StatusGone, map[string]string{"error": "diagnostic_stream_closed"})
		return
	}
	if errors.Is(err, telemetry.ErrDiagnosticStale) || errors.Is(err, telemetry.ErrDiagnosticConflict) || errors.Is(err, telemetry.ErrDiagnosticSequence) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "diagnostic_identity_conflict"})
		return
	}
	if err != nil {
		mapped := commandError(err, commandLogAppendOperation)
		if errorStatus(mapped) == http.StatusServiceUnavailable {
			s.log.Error("append command log failed", "command_id", request.CommandID, "error", err)
		}
		writeError(w, mapped)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.DiagnosticLogReceipt{Expired: receipt.Expired, ThroughSequence: receipt.ThroughSequence, AcceptedAt: receipt.AcceptedAt, ExpiresAt: receipt.ExpiresAt})
}

// commandLogChunk reads the canonical identities of a log record and bounds
// its content; the command owner validates its observation.
func commandLogChunk(request workerapi.CommandLogAppendRequest) (command.LogChunk, error) {
	environment, e1 := ids.Parse(request.EnvironmentID)
	commandID, e2 := ids.Parse(request.CommandID)
	instance, e3 := ids.Parse(request.ComputerInstanceID)
	if e1 != nil || e2 != nil || e3 != nil {
		return command.LogChunk{}, command.ErrInvalidLog
	}
	return command.LogChunk{
		EnvironmentID: environment, CommandID: commandID, InstanceID: instance, WriterGeneration: request.WriterGeneration,
		Kind: request.Kind, ThroughSequence: request.ThroughSequence, DroppedBytes: request.DroppedBytes, Complete: request.Complete, Stream: string(request.Stream), ObservedSeq: request.ObservedSeq, ObservedAt: request.ObservedAt, Content: request.Content,
	}, nil
}
