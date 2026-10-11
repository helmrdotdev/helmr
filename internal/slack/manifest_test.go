package slack

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"uuid"
)

func TestDedicatedAppManifestSeparatesBotInstallationAndIdentityConsent(t *testing.T) {
	const expectedScopes = "app_mentions:read,channels:read,channels:history,groups:read,groups:history,chat:write,assistant:write"
	origin, _ := url.Parse("https://helmr.example")
	oauth := OAuthClient{clientID: "client"}
	authorization, err := url.Parse(oauth.AuthorizationURL("state", "https://helmr.example/auth/slack/callback"))
	if err != nil {
		t.Fatal(err)
	}
	if authorization.Query().Get("scope") != expectedScopes || authorization.Query().Get("user_scope") != "" {
		t.Fatal("bot OAuth scope drift", authorization.Query())
	}
	registration := uuid.NewV7()
	for _, name := range []string{"My Agent", "日本語のエージェント", strings.Repeat("x", 36)} {
		var manifest struct {
			Display struct {
				Name string `json:"name"`
			} `json:"display_information"`
			Features struct {
				Bot struct {
					Name string `json:"display_name"`
				} `json:"bot_user"`
				Agent struct {
					Description string `json:"agent_description"`
				} `json:"agent_view"`
			} `json:"features"`
			OAuth struct {
				Redirects []string            `json:"redirect_urls"`
				Scopes    map[string][]string `json:"scopes"`
			} `json:"oauth_config"`
			Settings struct {
				Events struct {
					URL string `json:"request_url"`
				} `json:"event_subscriptions"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(AppManifest(origin, registration, name), &manifest); err != nil {
			t.Fatal(err)
		}
		if len([]rune(manifest.Display.Name)) > 35 || manifest.Features.Bot.Name == "" || strings.ContainsAny(manifest.Features.Bot.Name, " ABCDEFGHIJKLMNOPQRSTUVWXYZ") || manifest.Features.Agent.Description == "" {
			t.Fatal("invalid display or Agent feature", manifest.Features)
		}
		if strings.Join(manifest.OAuth.Scopes["bot"], ",") != expectedScopes || len(manifest.OAuth.Scopes) != 1 {
			t.Fatal("installation scope drift", manifest.OAuth.Scopes)
		}
		if len(manifest.OAuth.Redirects) != 2 || manifest.OAuth.Redirects[1] != "https://helmr.example/api/slack/user-links/callback" || !strings.Contains(manifest.Settings.Events.URL, registration.String()) {
			t.Fatal("callback scope drift", manifest)
		}
	}
}
