package control

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func (s *Server) promoteRuntimeRelease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("runtime release storage is not configured"))
		return
	}
	var request api.PromoteRuntimeReleaseRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid runtime release promotion JSON: %w", err))
		return
	}
	request.RuntimeID = strings.TrimSpace(request.RuntimeID)
	if request.RuntimeID == "" {
		writeError(w, http.StatusBadRequest, errors.New("runtime_id is required"))
		return
	}
	current, err := s.db.PromoteCurrentRuntimeRelease(r.Context(), request.RuntimeID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, errors.New("runtime release is not observed"))
		return
	}
	if err != nil {
		s.log.Error("promote runtime release failed", "runtime_id", request.RuntimeID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("promote runtime release"))
		return
	}
	writeJSON(w, http.StatusOK, runtimeReleaseResponse(current))
}

func runtimeReleaseResponse(current db.CurrentRuntimeRelease) api.RuntimeReleaseResponse {
	return api.RuntimeReleaseResponse{
		RuntimeID:  current.RuntimeID,
		SelectedAt: pgTime(current.SelectedAt),
	}
}
