package controlplane

import (
	"errors"
	"net/http"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/audit"
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

// Retain the initiating browser authority across the provider redirect.
type slackManagementSource struct {
	SessionID       uuid.UUID `json:"session_id"`
	MemberID        uuid.UUID `json:"member_id"`
	IdentityID      uuid.UUID `json:"identity_id"`
	PasskeyID       uuid.UUID `json:"passkey_id"`
	AccessEpoch     int64     `json:"access_epoch"`
	AuthenticatedAt time.Time `json:"authenticated_at"`
}

func slackManagementSourceFor(p auth.Principal) *slackManagementSource {
	return &slackManagementSource{SessionID: p.SessionID, MemberID: p.MemberID, IdentityID: p.IdentityID, PasskeyID: p.PasskeyID, AccessEpoch: p.AccessEpoch, AuthenticatedAt: p.AuthenticatedAt}
}
func (s *slackManagementSource) matches(p auth.Principal) bool {
	return s != nil && s.SessionID == p.SessionID && s.MemberID == p.MemberID && s.IdentityID == p.IdentityID && s.PasskeyID == p.PasskeyID && s.AccessEpoch == p.AccessEpoch && s.AuthenticatedAt.Equal(p.AuthenticatedAt)
}

const browserAuthSlackInstallation browserAuthKind = "slack_installation"

func (s *Server) mountSlackRoutes(r chi.Router) {
	r.Get("/slack/user-links/callback", s.slackUserLinkCallback)
	r.Post("/slack/user-links/callback", s.slackUserLinkCallback)
	r.With(s.requireAccount).Get("/slack/user-links/first-use", s.getSlackFirstUseLink)

	r.Group(func(r chi.Router) {
		r.Use(s.requireSession)
		r.Get("/slack/user-links", s.listSlackUserLinks)
		r.Get("/slack/link-workspaces", s.listSlackLinkWorkspaces)
		r.Post("/slack/user-links/authorize", s.startSlackUserLink)
		r.Post("/slack/user-links/verify", s.verifySlackUserLink)
		r.Post("/slack/user-links/confirm", s.confirmSlackUserLink)
		r.Delete("/slack/user-links/{teamID}/{slackUserID}", s.unlinkSlackUser)

		r.Post("/slack/installations/finish", s.finishSlackAuthorization)
	})
}

func slackManagementPrincipal(r *http.Request) (auth.Principal, error) {
	principal := principalFromContext(r.Context())
	if principal.UserID == uuid.Nil() || principal.OrgID == uuid.Nil() || principal.Kind != auth.PrincipalKindSession || principal.Channel != "browser" {
		return principal, forbidden(errors.New("a human organization member is required"))
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
	if err != nil || flow.Kind != browserAuthSlackInstallation || !flow.SlackManagementSource.matches(principal) || request.State == "" || request.State != flow.State || flow.OrganizationID != principal.OrgID.String() || flow.UserID != principal.UserID.String() {
		writeError(w, badRequest(errors.New("the Slack authorization flow expired or account changed; start again")))
		return
	}
	clearAuthFlowCookie(w, r)
	if request.Error != "" || request.Code == "" {
		writeError(w, badRequest(errors.New("the Slack authorization was not completed")))
		return
	}
	connection, err := ids.Parse(flow.ConnectionID)
	if err != nil || connection == uuid.Nil() {
		writeError(w, badRequest(errors.New("invalid Agent connection")))
		return
	}
	installation, err := s.slackConfig.Credentials.AuthorizeConnection(r.Context(), principal, connection, request.Code, s.slackRedirectURL(), s.slackConfig.Transport)
	if err != nil {
		s.writeSlackError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"installation_id": installation.String(), "return_url": flow.RedirectAfter})
}

func (s *Server) slackRedirectURL() string {
	return s.appOrigin.JoinPath("auth/slack/callback").String()
}

func (s *Server) writeSlackError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, audit.ErrCapture):
		s.writeOperationError(w, r, err)
	case slack.IsInstallationDenied(err):
		s.writeOperationError(w, r, forbidden(errors.New("the Slack connection is unavailable or Agent management authority changed")))
	case errors.Is(err, slack.ErrConnectionUnavailable):
		s.writeOperationError(w, r, conflict(err))
	case errors.Is(err, slack.ErrOAuthAuthorization):
		s.writeOperationError(w, r, badRequest(err))
	default:
		s.writeOperationError(w, r, errors.New("the Slack operation could not be completed"))
	}
}
