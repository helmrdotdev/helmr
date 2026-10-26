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

// These routes belong to the browser Session surface. Recovery grants no API-key
// mutation capability and requires Session read plus the exact delivery action.
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
	if r.Method == http.MethodGet {
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
		return
	}
	if _, err = slackManagementPrincipal(r); err != nil {
		writeError(w, err)
		return
	}
	post, err := ids.Parse(chi.URLParam(r, "postID"))
	if err != nil || post == uuid.Nil() {
		s.writeAgentHTTPError(w, r, session.ErrInvalidInput)
		return
	}
	if chi.URLParam(r, "recovery") == "check" && s.slackConfig == nil {
		writeError(w, unavailable(errors.New("the Slack integration is not configured")))
		return
	}
	var input slack.DeliveryRecovery
	if err = decodeRequestJSON(r, &input); err != nil {
		writeError(w, err)
		return
	}
	err = slack.RecoverDelivery(r.Context(), s.auditPolicy, s.tx, principal, environment, sessionID, post, input, chi.URLParam(r, "recovery"))
	if err != nil {
		s.writeSlackDeliveryError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) writeSlackDeliveryError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, audit.ErrCapture) {
		s.writeOperationError(w, r, err)
		return
	}
	switch {
	case errors.Is(err, slack.ErrDeliveryUnavailable):
		s.writeOperationError(w, r, notFound(err))
	case errors.Is(err, slack.ErrDeliveryChanged):
		s.writeOperationError(w, r, conflict(err))
	case errors.Is(err, slack.ErrDeliveryCheckUnavailable):
		s.writeOperationError(w, r, badRequest(err))
	default:
		s.writeSlackError(w, r, err)
	}
}
