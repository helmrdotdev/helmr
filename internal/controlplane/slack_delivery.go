package controlplane

import (
	"errors"
	"net/http"
	"strconv"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slack"
)

// Delivery evidence uses the same read authority as the Session history.
func (s *Server) sessionSlackDelivery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, environment, err := s.agentRequestAuthority(r, auth.PermissionSessionsRead)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	principal := principalFromContext(r.Context())
	sessionID, err := ids.Parse(chi.URLParam(r, "sessionID"))
	if err != nil || sessionID == uuid.Nil() {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}

	limit, err := agentPageLimit(r)
	if err != nil {
		s.writeAgentHTTPError(w, r, err)
		return
	}
	var after int64
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		after, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || after < 0 {
			s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
			return
		}
	}
	rows, err := slack.ListSessionDelivery(r.Context(), s.auditPolicy, s.tx, principal, environment, sessionID, after, limit)
	if err != nil {
		s.writeSlackDeliveryError(w, r, err)
		return
	}
	page := map[string]any{"posts": rows}
	if len(rows) == limit {
		page["next_cursor"] = strconv.FormatInt(rows[len(rows)-1].Sequence, 10)
	}
	writeJSON(w, http.StatusOK, page)
}
func (s *Server) writeSlackDeliveryError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, audit.ErrCapture) {
		s.writeOperationError(w, r, err)
		return
	}
	switch {
	case errors.Is(err, slack.ErrDeliveryUnavailable):
		s.writeOperationError(w, r, notFound(err))
	default:
		s.writeSlackError(w, r, err)
	}
}
