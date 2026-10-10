package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/slack"
)

type SlackConfig struct {
	Transport   http.RoundTripper
	ControlKey  []byte
	Credentials *slack.CredentialStore
	Client      *slack.WebClient
}

const browserAuthSlackInstallation browserAuthKind = "slack_installation"

func (s *Server) mountSlackRoutes(r chi.Router) {
	r.Get("/slack/user-links/callback", s.slackUserLinkCallback)
	r.Post("/slack/user-links/callback", s.slackUserLinkCallback)

	r.Group(func(r chi.Router) {
		r.Use(s.requireSession)
		r.Get("/slack/user-links", s.listSlackUserLinks)
		r.Get("/slack/link-workspaces", s.listSlackLinkWorkspaces)
		r.Get("/slack/user-links/first-use", s.getSlackFirstUseLink)
		r.Post("/slack/user-links/authorize", s.startSlackUserLink)
		r.Post("/slack/user-links/verify", s.verifySlackUserLink)
		r.Post("/slack/user-links/confirm", s.confirmSlackUserLink)
		r.Delete("/slack/user-links/{teamID}/{slackUserID}", s.unlinkSlackUser)

		r.Post("/slack/installations/finish", s.finishSlackAuthorization)
	})
}

func slackManagementPrincipal(r *http.Request) (auth.Principal, error) {
	principal := principalFromContext(r.Context())
	if principal.UserID == uuid.Nil() || principal.OrgID == uuid.Nil() || (principal.Role != auth.RoleOwner && principal.Role != auth.RoleAdmin) {
		return principal, forbidden(errors.New("organization management is required"))
	}
	return principal, nil
}

func (s *Server) finishSlackAuthorization(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal, err := slackManagementPrincipal(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.slackConfig == nil {
		writeError(w, unavailable(errors.New("the Slack integration is not configured")))
		return
	}
	var request struct {
		State string `json:"state"`
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(errors.New("invalid Slack callback")))
		return
	}
	flow, err := s.decodeAuthFlow(r)
	if err != nil || flow.Kind != browserAuthSlackInstallation || request.State == "" || request.State != flow.State || flow.OrganizationID != principal.OrgID.String() || flow.UserID != principal.UserID.String() {
		writeError(w, badRequest(errors.New("the Slack authorization flow expired or account changed; start again")))
		return
	}
	clearAuthFlowCookie(w, r)
	if request.Error != "" || request.Code == "" {
		writeError(w, badRequest(errors.New("the Slack authorization was not completed")))
		return
	}
	publication, err := ids.Parse(flow.PublicationID)
	if err != nil || publication == uuid.Nil() {
		writeError(w, badRequest(errors.New("invalid Agent connection")))
		return
	}
	installation, err := s.slackConfig.Credentials.AuthorizePublication(r.Context(), principal.OrgID, principal.UserID, publication, request.Code, s.slackRedirectURL(), s.slackConfig.Transport)
	if err != nil {
		s.writeSlackError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"installation_id": installation.String(), "return_url": flow.RedirectAfter})
}

func (s *Server) slackRedirectURL() string {
	return s.publicURL.JoinPath("auth/slack/callback").String()
}

func (s *Server) writeSlackError(w http.ResponseWriter, err error) {
	switch {
	case slack.IsInstallationDenied(err):
		writeError(w, forbidden(errors.New("the Slack connection is unavailable or organization management authority changed")))
	case errors.Is(err, slack.ErrPublicationUnavailable):
		writeError(w, conflict(err))
	case errors.Is(err, slack.ErrOAuthAuthorization):
		writeError(w, badRequest(err))
	default:
		writeError(w, errors.New("the Slack operation could not be completed"))
	}
}
