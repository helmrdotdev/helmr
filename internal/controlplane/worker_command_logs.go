package controlplane

import (
	"net/http"

	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Base64 payload plus the bounded execution identity and timestamp envelope.
const workerCommandLogRequestBodyLimit = int64(1024 + (telemetry.MaxRunLogContentBytes+2)/3*4)

func (s *Server) workerAppendCommandLogs(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CommandLogAppendRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	chunk, err := commandLogChunk(request)
	if err == nil {
		err = command.AppendLog(r.Context(), s.tx, workerFromContext(r.Context()), chunk)
	}
	if err != nil {
		mapped := commandError(err, commandLogAppendOperation)
		if errorStatus(mapped) == http.StatusServiceUnavailable {
			s.log.Error("append command log failed", "command_id", request.CommandID, "error", err)
		}
		writeError(w, mapped)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// commandLogChunk reads the canonical identities of a log record and bounds
// its content; the command owner validates its observation.
func commandLogChunk(request workerapi.CommandLogAppendRequest) (command.LogChunk, error) {
	org, e1 := ids.Parse(request.OrgID)
	commandID, e2 := ids.Parse(request.CommandID)
	instance, e3 := ids.Parse(request.ComputerInstanceID)
	if e1 != nil || e2 != nil || e3 != nil || telemetry.ValidateRunLog(request.Content) != nil {
		return command.LogChunk{}, command.ErrInvalidLog
	}
	return command.LogChunk{
		OrgID: org, CommandID: commandID, InstanceID: instance, WriterGeneration: request.WriterGeneration,
		Stream: string(request.Stream), ObservedSeq: request.ObservedSeq, ObservedAt: request.ObservedAt, Content: request.Content,
	}, nil
}
