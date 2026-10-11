package controlplane

import (
	"errors"
	"net/http"
	"net/url"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/slack"
)

func (s *Server) agentSlackPublication(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal, err := slackManagementPrincipal(r)
	if err != nil {
		writeError(w, err)
		return
	}
	_, _, environment, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.slackConfig == nil {
		writeError(w, unavailable(errors.New("the Slack integration is not configured")))
		return
	}
	env := pgvalue.MustUUIDValue(environment)
	name := chi.URLParam(r, "agentName")
	agent, err := slack.PublicationAgent(r.Context(), s.tx, principal.OrgID, principal.UserID, env, name)
	if err != nil {
		s.writeSlackError(w, err)
		return
	}
	var publication uuid.UUID
	if raw := chi.URLParam(r, "publicationID"); raw != "" {
		publication, err = ids.Parse(raw)
		if err != nil || publication == uuid.Nil() {
			writeError(w, badRequest(errors.New("invalid Agent connection")))
			return
		}
	}
	if r.Method == http.MethodPost && publication == uuid.Nil() {
		_, err = slack.BeginPublication(r.Context(), s.tx, principal.OrgID, principal.UserID, env, agent)
		if err != nil {
			s.writeSlackError(w, err)
			return
		}
	}
	view, err := slack.GetPublication(r.Context(), s.tx, principal.OrgID, principal.UserID, env, agent)
	if err != nil {
		s.writeSlackError(w, err)
		return
	}
	if publication != uuid.Nil() && (view == nil || view.ID != publication) {
		s.writeSlackError(w, slack.ErrPublicationUnavailable)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var credentials slack.AppCredentials
		if err = decodeRequestJSON(r, &credentials); err != nil {
			writeError(w, badRequest(errors.New("invalid app credentials")))
			return
		}
		if err = s.slackConfig.Credentials.StoreAppCredentials(r.Context(), principal.OrgID, principal.UserID, view.RegistrationID, credentials); err != nil {
			s.writeSlackError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodDelete:
		if err = s.slackConfig.Credentials.DisconnectPublication(r.Context(), principal.OrgID, principal.UserID, env, publication); err != nil {
			s.writeSlackError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
		if publication != uuid.Nil() {
			var input struct {
				ReturnTo string `json:"return_to"`
			}
			if err = decodeRequestJSON(r, &input); err != nil {
				writeError(w, badRequest(errors.New("invalid authorization request")))
				return
			}
			state, err := auth.GenerateOpaque(32)
			if err != nil {
				writeError(w, err)
				return
			}
			redirect, err := s.slackConfig.Credentials.PublicationAuthorizationURL(r.Context(), principal.OrgID, principal.UserID, publication, state, s.slackRedirectURL())
			if err != nil {
				s.writeSlackError(w, err)
				return
			}
			flow := browserAuthFlow{Kind: browserAuthSlackInstallation, State: state, OrganizationID: principal.OrgID.String(), UserID: principal.UserID.String(), PublicationID: publication.String(), AppRegistrationID: view.RegistrationID.String(), EnvironmentID: env.String(), RedirectAfter: validateRedirectAfter(input.ReturnTo)}
			encoded, err := s.encodeAuthFlow(flow)
			if err != nil {
				writeError(w, err)
				return
			}
			http.SetCookie(w, authFlowCookie(r, encoded, int(authFlowTTL.Seconds())))
			w.Header().Set("Referrer-Policy", "no-referrer")
			writeJSON(w, http.StatusOK, map[string]string{"redirect_url": redirect})
			return
		}
	}
	result := map[string]any{"publication": view}
	if view != nil {
		manifest := slack.AppManifest(s.publicURL, view.RegistrationID, name)
		result["manifest"] = manifest
		result["create_app_url"] = "https://api.slack.com/apps?" + url.Values{"new_app": {"1"}, "manifest": {string(manifest)}}.Encode()
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}
