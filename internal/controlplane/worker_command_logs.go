package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

// Base64 payload plus the bounded execution identity and timestamp envelope.
const workerCommandLogRequestBodyLimit = int64(1024 + (telemetry.MaxRunLogContentBytes+2)/3*4)

func (s *Server) workerAppendCommandLogs(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CommandLogAppendRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	worker := workerFromContext(r.Context())
	err := s.inTx(r.Context(), func(work *txWork) error {
		return appendCommandLog(r.Context(), work.tx, worker, request)
	})
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, errInvalidCommandLog) {
		writeError(w, badRequest(err))
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("command log producer is stale or sequence contains different content")))
		return
	}
	if err != nil {
		s.log.Error("append command log failed", "command_id", request.CommandID, "error", err)
		writeError(w, unavailable(errors.New("append command log failed")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
