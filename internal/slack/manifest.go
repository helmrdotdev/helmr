package slack

import (
	"encoding/json"
	"net/url"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slackroute"
)

// AppManifest and authorization share the exact bot scope set. User identity
// uses separate OpenID consent and is never inferred from bot installation.
func AppManifest(appOrigin *url.URL, registration uuid.UUID, name string, mode Transport) json.RawMessage {
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
	events := appOrigin.JoinPath("integrations/slack/apps", registration.String(), "events").String()
	interactions := appOrigin.JoinPath("integrations/slack/apps", registration.String(), "interactions").String()
	settings := map[string]any{
		"event_subscriptions": map[string]any{"bot_events": []string{"agent_session_stopped", "message.im", "app_home_opened", "app_mention", "message.channels", "message.groups", "app_uninstalled", "tokens_revoked"}},
		"interactivity":       map[string]any{"is_enabled": true},
		"org_deploy_enabled":  false, "socket_mode_enabled": mode == TransportSocket, "token_rotation_enabled": true,
	}
	if mode == TransportHTTP {
		settings["event_subscriptions"].(map[string]any)["request_url"] = events
		settings["interactivity"].(map[string]any)["request_url"] = interactions
	}
	raw, _ := json.Marshal(map[string]any{
		"display_information": map[string]any{"name": name, "description": "Work with your Helmr Agent in Slack"},
		"features":            map[string]any{"bot_user": map[string]any{"display_name": botName, "always_online": true}, "app_home": map[string]any{"home_tab_enabled": false, "messages_tab_enabled": true, "messages_tab_read_only_enabled": false}, "agent_view": map[string]any{"agent_description": directMessageDescription}},
		"oauth_config":        map[string]any{"redirect_urls": []string{appOrigin.JoinPath("auth/slack/callback").String(), appOrigin.JoinPath("api/slack/user-links/callback").String()}, "scopes": map[string]any{"bot": slackroute.RequiredScopes()}},
		"settings":            settings,
	})
	return raw
}
