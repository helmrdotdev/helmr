package controlplane

import (
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/slack"
)

func (s *Server) slackAppEvents(w http.ResponseWriter, r *http.Request) {
	s.slackAppCallback(w, r, false)
}
func (s *Server) slackAppInteractions(w http.ResponseWriter, r *http.Request) {
	s.slackAppCallback(w, r, true)
}
func (s *Server) slackAppCallback(w http.ResponseWriter, r *http.Request, interactive bool) {
	if s.slackConfig == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	registration, err := ids.Parse(chi.URLParam(r, "registrationID"))
	if err != nil || registration == uuid.Nil() {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	events := slack.EventHandler{Database: s.tx}
	var interactions *slack.InteractionHandler
	if interactive {
		interactions = &slack.InteractionHandler{PublicURL: s.publicURL, Database: s.tx, ControlKey: s.slackConfig.ControlKey, Client: s.slackConfig.Client}
	}
	s.slackConfig.Credentials.CallbackHandler(registration, events, interactions).ServeHTTP(w, r)
}
