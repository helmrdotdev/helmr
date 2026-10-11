package controlplane

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/slack"
)

const browserAuthSlackUser browserAuthKind = "slack_user"
const browserAuthSlackUserConfirm browserAuthKind = "slack_user_confirm"

func slackLinkPrincipal(r *http.Request) (auth.Principal, error) {
	p := principalFromContext(r.Context())
	if p.UserID == uuid.Nil() || p.OrgID == uuid.Nil() {
		return p, forbidden(errors.New("sign in to link your Slack identity"))
	}
	return p, nil
}
func (s *Server) listSlackUserLinks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, err := slackLinkPrincipal(r)
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := agentPageLimit(r)
	if err != nil {
		writeError(w, badRequest(errors.New("invalid page size")))
		return
	}
	after := ""
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		user, team, ok := strings.Cut(string(raw), ":")
		if e != nil || !ok || user != p.UserID.String() || team == "" || len(team) > 100 {
			writeError(w, badRequest(errors.New("invalid cursor")))
			return
		}
		after = team
	}
	links, err := slack.ListUserLinks(r.Context(), s.tx, p.OrgID, p.UserID, after, int(limit))
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	result := map[string]any{"links": links}
	if len(links) == int(limit) {
		result["next_cursor"] = base64.RawURLEncoding.EncodeToString([]byte(p.UserID.String() + ":" + links[len(links)-1].TeamID))
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) listSlackLinkWorkspaces(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, err := slackLinkPrincipal(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.slackConfig == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "workspaces": []slack.LinkWorkspace{}})
		return
	}
	limit, err := agentPageLimit(r)
	if err != nil {
		writeError(w, badRequest(errors.New("invalid page size")))
		return
	}
	var before uuid.UUID
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		org, id, ok := strings.Cut(string(raw), ":")
		if e != nil || !ok || org != p.OrgID.String() {
			writeError(w, badRequest(errors.New("invalid cursor")))
			return
		}
		before, err = ids.Parse(id)
		if err != nil || before == uuid.Nil() {
			writeError(w, badRequest(errors.New("invalid cursor")))
			return
		}
	}
	values, err := slack.ListLinkWorkspaces(r.Context(), s.tx, p.OrgID, p.UserID, before, int(limit))
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	result := map[string]any{"configured": true, "workspaces": values}
	if len(values) == int(limit) {
		result["next_cursor"] = base64.RawURLEncoding.EncodeToString([]byte(p.OrgID.String() + ":" + values[len(values)-1].InstallationID.String()))
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) startSlackUserLink(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if s.slackConfig == nil {
		writeError(w, unavailable(errors.New("the Slack integration is not configured")))
		return
	}
	var input struct {
		InstallationID string `json:"installation_id"`
		Link           string `json:"link"`
	}
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, err)
		return
	}
	var origin slack.FirstUseLink
	var installation uuid.UUID
	var err error
	if input.Link != "" && input.InstallationID == "" {
		origin, err = slack.ResolveFirstUseLink(r.Context(), s.tx, s.slackConfig.ControlKey, input.Link, p.UserID)
		if err != nil {
			s.writeSlackLinkError(w, err)
			return
		}
		p.OrgID = origin.OrganizationID
		installation = origin.InstallationID
	} else if input.Link == "" && input.InstallationID != "" {
		p, err = slackLinkPrincipal(r)
		if err != nil {
			writeError(w, err)
			return
		}
		installation, err = ids.Parse(input.InstallationID)
	}
	if err != nil || installation == uuid.Nil() {
		writeError(w, badRequest(errors.New("select a connected Slack workspace")))
		return
	}
	workspace, err := slack.CheckLinkWorkspace(r.Context(), s.tx, p.OrgID, p.UserID, installation)
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	registration, oauth, err := s.slackConfig.Credentials.InstallationOAuth(r.Context(), installation, s.slackConfig.Transport)
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	state, err := auth.GenerateOpaque(32)
	if err != nil {
		writeError(w, err)
		return
	}
	nonce, err := auth.GenerateOpaque(32)
	if err != nil {
		writeError(w, err)
		return
	}
	flow := browserAuthFlow{Kind: browserAuthSlackUser, State: state, Nonce: nonce, OrganizationID: p.OrgID.String(), UserID: p.UserID.String(), InstallationID: installation.String(), AppRegistrationID: registration.String(), SlackTeamID: workspace.TeamID, SlackUserID: origin.SlackUserID, SlackLinkToken: input.Link, SlackReturnURL: origin.ReturnURL}
	encoded, err := s.encodeAuthFlow(flow)
	if err != nil {
		writeError(w, err)
		return
	}
	http.SetCookie(w, authFlowCookie(r, encoded, int(authFlowTTL.Seconds())))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusOK, map[string]string{"redirect_url": oauth.IdentityAuthorizationURL(state, nonce, workspace.TeamID, s.slackUserRedirectURL())})
}
func (s *Server) slackUserRedirectURL() string {
	return s.publicURL.JoinPath("api/slack/user-links/callback").String()
}

// Slack can return an authorization code by query or form_post. Cross-site POSTs
// withhold Lax authentication cookies. This untrusted bridge only redirects to
// the same-site confirmation page; it never exchanges a code or changes a link.
// The URL fragment avoids
// sending the temporary code to the Console server or in later referrers.
func (s *Server) slackUserLinkCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	var fields url.Values
	var err error
	if r.Method == http.MethodGet {
		if len(r.URL.RawQuery) > 8192 {
			writeError(w, badRequest(errors.New("invalid Slack identity callback")))
			return
		}
		fields, err = url.ParseQuery(r.URL.RawQuery)
	} else {
		limitRequestBodySize(w, r, 8192)
		fields, err = decodeRequestForm(r)
	}
	if err != nil {
		writeError(w, badRequest(errors.New("invalid Slack identity callback")))
		return
	}
	values := url.Values{}
	for _, key := range []string{"state", "code", "error"} {
		items := fields[key]
		if len(items) > 1 {
			writeError(w, badRequest(errors.New("invalid Slack identity callback")))
			return
		}
		if len(items) == 1 {
			if len(items[0]) > 4096 {
				writeError(w, badRequest(errors.New("invalid Slack identity callback")))
				return
			}
			values.Set(key, items[0])
		}
	}
	target := *s.publicURL.JoinPath("auth/slack/link")
	target.Fragment = values.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}
func (s *Server) verifySlackUserLink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := principalFromContext(r.Context())
	if s.slackConfig == nil {
		writeError(w, unavailable(errors.New("the Slack integration is not configured")))
		return
	}
	var input struct {
		State string `json:"state"`
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, err)
		return
	}
	flow, err := s.decodeAuthFlow(r)
	if err != nil || flow.Kind != browserAuthSlackUser || input.State == "" || input.State != flow.State || flow.UserID != p.UserID.String() || (flow.SlackLinkToken == "" && flow.OrganizationID != p.OrgID.String()) {
		writeError(w, badRequest(errors.New("the Slack identity flow expired or account changed; start linking again")))
		return
	}
	if err := s.resolveSlackLinkFlow(r, &p, flow); err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	clearAuthFlowCookie(w, r)
	if input.Error != "" || input.Code == "" {
		writeError(w, badRequest(slack.ErrUserIdentity))
		return
	}
	installation, err := ids.Parse(flow.InstallationID)
	if err != nil {
		writeError(w, badRequest(slack.ErrUserIdentity))
		return
	}
	workspace, err := slack.CheckLinkWorkspace(r.Context(), s.tx, p.OrgID, p.UserID, installation)
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	registration, oauth, err := s.slackConfig.Credentials.InstallationOAuth(r.Context(), installation, s.slackConfig.Transport)
	if err != nil || registration.String() != flow.AppRegistrationID {
		s.writeSlackLinkError(w, slack.ErrUserIdentity)
		return
	}
	identity, err := oauth.VerifyIdentity(r.Context(), input.Code, s.slackUserRedirectURL(), flow.Nonce, flow.SlackTeamID)
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	if flow.SlackUserID != "" && identity.SlackUserID != flow.SlackUserID {
		writeError(w, badRequest(errors.New("this is a different Slack account; return to Slack and link using the account that requested work")))
		return
	}
	account, err := s.db.GetUserOnboardingState(r.Context(), db.GetUserOnboardingStateParams{OrgID: pgvalue.UUID(p.OrgID), UserID: pgvalue.UUID(p.UserID)})
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	state, err := auth.GenerateOpaque(32)
	if err != nil {
		writeError(w, err)
		return
	}
	confirmation := browserAuthFlow{Kind: browserAuthSlackUserConfirm, State: state, OrganizationID: p.OrgID.String(), UserID: p.UserID.String(), InstallationID: flow.InstallationID, AppRegistrationID: flow.AppRegistrationID, SlackTeamID: identity.TeamID, SlackUserID: identity.SlackUserID, SlackLinkToken: flow.SlackLinkToken, SlackReturnURL: flow.SlackReturnURL}
	encoded, err := s.encodeAuthFlow(confirmation)
	if err != nil {
		writeError(w, err)
		return
	}
	http.SetCookie(w, authFlowCookie(r, encoded, int(authFlowTTL.Seconds())))
	writeJSON(w, http.StatusOK, map[string]any{"identity": identity, "confirmation": state, "helmr_user_id": p.UserID.String(), "helmr_display_name": account.DisplayName, "helmr_org_name": account.OrgName.String, "workspace_name": workspace.WorkspaceName, "return_url": flow.SlackReturnURL})
}
func (s *Server) confirmSlackUserLink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := principalFromContext(r.Context())
	var input struct {
		Confirmation string `json:"confirmation"`
		Confirmed    bool   `json:"confirmed"`
	}
	if err := decodeRequestJSON(r, &input); err != nil {
		writeError(w, err)
		return
	}
	flow, err := s.decodeAuthFlow(r)
	if err != nil || flow.Kind != browserAuthSlackUserConfirm || input.Confirmation == "" || input.Confirmation != flow.State || !input.Confirmed || flow.UserID != p.UserID.String() || (flow.SlackLinkToken == "" && flow.OrganizationID != p.OrgID.String()) {
		writeError(w, badRequest(errors.New("confirm the verified Slack identity using the account that started linking")))
		return
	}
	if err := s.resolveSlackLinkFlow(r, &p, flow); err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	installation, err := ids.Parse(flow.InstallationID)
	if err != nil {
		writeError(w, badRequest(slack.ErrUserIdentity))
		return
	}
	err = slack.LinkUser(r.Context(), s.tx, p.OrgID, p.UserID, installation, slack.UserIdentity{TeamID: flow.SlackTeamID, SlackUserID: flow.SlackUserID})
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	clearAuthFlowCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) unlinkSlackUser(w http.ResponseWriter, r *http.Request) {
	p, err := slackLinkPrincipal(r)
	if err != nil {
		writeError(w, err)
		return
	}
	err = slack.UnlinkUser(r.Context(), s.tx, p.OrgID, p.UserID, chi.URLParam(r, "teamID"), chi.URLParam(r, "slackUserID"))
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) writeSlackLinkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, slack.ErrFirstUseLink):
		writeError(w, badRequest(err))
	case errors.Is(err, slack.ErrUserIdentity):
		writeError(w, badRequest(err))
	case errors.Is(err, slack.ErrUserLinkDenied):
		writeError(w, forbidden(err))
	case errors.Is(err, slack.ErrUserLinkConflict):
		writeError(w, conflict(err))
	default:
		s.writeSlackError(w, err)
	}
}

func (s *Server) getSlackFirstUseLink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if s.slackConfig == nil {
		writeError(w, unavailable(errors.New("the Slack integration is not configured")))
		return
	}
	link, err := slack.ResolveFirstUseLink(r.Context(), s.tx, s.slackConfig.ControlKey, r.URL.Query().Get("link"), principalFromContext(r.Context()).UserID)
	if err != nil {
		s.writeSlackLinkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, link)
}

func (s *Server) resolveSlackLinkFlow(r *http.Request, p *auth.Principal, flow browserAuthFlow) error {
	if flow.SlackLinkToken == "" {
		return nil
	}
	if s.slackConfig == nil {
		return slack.ErrFirstUseLink
	}
	origin, err := slack.ResolveFirstUseLink(r.Context(), s.tx, s.slackConfig.ControlKey, flow.SlackLinkToken, p.UserID)
	if err != nil {
		return err
	}
	if origin.OrganizationID.String() != flow.OrganizationID || origin.InstallationID.String() != flow.InstallationID || origin.TeamID != flow.SlackTeamID || origin.SlackUserID != flow.SlackUserID || origin.ReturnURL != flow.SlackReturnURL {
		return slack.ErrFirstUseLink
	}
	p.OrgID = origin.OrganizationID
	return nil
}
