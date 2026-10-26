package controlplane

import (
	"errors"
	"net/http"
	"net/url"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/slack"
)

func (s *Server) agentSlackConnection(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal, err := slackManagementPrincipal(r)
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	_, _, environment, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		s.writeOperationError(w, r, err)
		return
	}
	env := pgvalue.MustUUIDValue(environment)
	name := chi.URLParam(r, "agentName")
	permission := auth.PermissionAgentConnectionsRead
	switch r.Method {
	case http.MethodPut:
		permission = auth.PermissionAgentConnectionsUpdateCredentials
	case http.MethodDelete:
		permission = auth.PermissionAgentConnectionsDelete
	case http.MethodPost:
		permission = auth.PermissionAgentConnectionsCreate
		if chi.URLParam(r, "connectionID") != "" {
			permission = auth.PermissionAgentConnectionsAuthorize
		}
	}
	agent, err := slack.ConnectionAgent(r.Context(), s.tx, principal.OrgID, principal.UserID, env, name, permission)
	if err != nil {
		s.writeSlackError(w, r, err)
		return
	}
	if s.slackConfig == nil {
		s.writeOperationError(w, r, unavailable(errors.New("the Slack integration is not configured")))
		return
	}
	var connection uuid.UUID
	if raw := chi.URLParam(r, "connectionID"); raw != "" {
		connection, err = ids.Parse(raw)
		if err != nil || connection == uuid.Nil() {
			s.writeOperationError(w, r, badRequest(errors.New("invalid Agent connection")))
			return
		}
	}
	if r.Method == http.MethodPost && connection == uuid.Nil() {
		_, err = slack.BeginConnection(r.Context(), s.tx, principal.OrgID, principal.UserID, env, agent, s.slackConfig.Credentials.DeliveryTransport())
		if err != nil {
			s.writeSlackError(w, r, err)
			return
		}
	}
	view, err := slack.GetConnection(r.Context(), s.tx, principal.OrgID, principal.UserID, env, agent, permission)
	if err != nil {
		s.writeSlackError(w, r, err)
		return
	}
	if connection != uuid.Nil() && (view == nil || view.ID != connection) {
		s.writeSlackError(w, r, slack.ErrConnectionUnavailable)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var credentials slack.AppCredentials
		if err = decodeRequestJSON(r, &credentials); err != nil {
			s.writeOperationError(w, r, badRequest(errors.New("invalid app credentials")))
			return
		}
		if err = s.slackConfig.Credentials.StoreAppCredentials(r.Context(), audit.FromPrincipal(principal), principal.OrgID, principal.UserID, view.RegistrationID, credentials); err != nil {
			s.writeSlackError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodDelete:
		if err = s.slackConfig.Credentials.DisconnectConnection(r.Context(), audit.FromPrincipal(principal), principal.OrgID, principal.UserID, env, connection); err != nil {
			s.writeSlackError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
		if connection != uuid.Nil() {
			var input struct {
				ReturnTo string `json:"return_to"`
			}
			if err = decodeRequestJSON(r, &input); err != nil {
				s.writeOperationError(w, r, badRequest(errors.New("invalid authorization request")))
				return
			}
			state, err := auth.GenerateOpaque(32)
			if err != nil {
				s.writeOperationError(w, r, err)
				return
			}
			redirect, err := s.slackConfig.Credentials.ConnectionAuthorizationURL(r.Context(), principal.OrgID, principal.UserID, connection, state, s.slackRedirectURL())
			if err != nil {
				s.writeSlackError(w, r, err)
				return
			}
			flow := browserAuthFlow{Kind: browserAuthSlackInstallation, State: state, OrganizationID: principal.OrgID.String(), UserID: principal.UserID.String(), ConnectionID: connection.String(), AppRegistrationID: view.RegistrationID.String(), EnvironmentID: env.String(), RedirectAfter: validateRedirectAfter(input.ReturnTo)}
			encoded, err := s.encodeAuthFlow(flow)
			if err != nil {
				s.writeOperationError(w, r, err)
				return
			}
			http.SetCookie(w, authFlowCookie(r, encoded, int(authFlowTTL.Seconds())))
			w.Header().Set("Referrer-Policy", "no-referrer")
			writeJSON(w, http.StatusOK, map[string]string{"redirect_url": redirect})
			return
		}
	}
	if view != nil && view.Transport != s.slackConfig.Credentials.DeliveryTransport() {
		view.Status = "configuration_mismatch"
	}
	result := map[string]any{"connection": view, "transport": s.slackConfig.Credentials.DeliveryTransport()}
	if view != nil {
		manifest := slack.AppManifest(s.appOrigin, view.RegistrationID, name, s.slackConfig.Credentials.DeliveryTransport())
		result["manifest"] = manifest
		result["create_app_url"] = "https://api.slack.com/apps?" + url.Values{"new_app": {"1"}, "manifest": {string(manifest)}}.Encode()
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}
