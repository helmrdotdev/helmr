package slack

import (
	"encoding/json"
	"net/url"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
)

// AppManifest and authorization share the exact bot scope set. User identity
// uses separate OpenID consent and is never inferred from bot installation.
func AppManifest(publicURL *url.URL, registration uuid.UUID, name string) json.RawMessage {
	if len([]rune(name)) > 35 {
		name = "Helmr Agent"
	}
	botName := strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(name)), "-")
	if botName == "" {
		botName = "helmr-agent"
	}
	events := publicURL.JoinPath("integrations/slack/apps", registration.String(), "events").String()
	interactions := publicURL.JoinPath("integrations/slack/apps", registration.String(), "interactions").String()
	raw, _ := json.Marshal(map[string]any{
		"display_information": map[string]any{"name": name, "description": "Work with your Helmr Agent in Slack"},
		"features":            map[string]any{"bot_user": map[string]any{"display_name": botName, "always_online": false}, "agent_view": map[string]any{"agent_description": "Work with your Helmr Agent in Slack"}},
		"oauth_config":        map[string]any{"redirect_urls": []string{publicURL.JoinPath("auth/slack/callback").String(), publicURL.JoinPath("api/slack/user-links/callback").String()}, "scopes": map[string]any{"bot": agent.RequiredSlackScopes()}},
		"settings":            map[string]any{"event_subscriptions": map[string]any{"request_url": events, "bot_events": []string{"app_mention", "message.channels", "message.groups", "app_uninstalled", "tokens_revoked"}}, "interactivity": map[string]any{"is_enabled": true, "request_url": interactions}, "org_deploy_enabled": false, "socket_mode_enabled": false, "token_rotation_enabled": true},
	})
	return raw
}
