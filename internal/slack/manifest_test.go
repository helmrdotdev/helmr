package slack

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
	"uuid"
)

func TestDedicatedAppManifestSeparatesBotInstallationAndIdentityConsent(t *testing.T) {
	const expectedScopes = "app_mentions:read,channels:read,channels:history,groups:read,groups:history,chat:write,assistant:write,im:history,im:read"
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
				Home struct {
					Messages bool `json:"messages_tab_enabled"`
					ReadOnly bool `json:"messages_tab_read_only_enabled"`
				} `json:"app_home"`
				Bot struct {
					Name         string `json:"display_name"`
					AlwaysOnline bool   `json:"always_online"`
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
					URL       string   `json:"request_url"`
					BotEvents []string `json:"bot_events"`
				} `json:"event_subscriptions"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(AppManifest(origin, registration, name, TransportHTTP), &manifest); err != nil {
			t.Fatal(err)
		}
		if !manifest.Features.Bot.AlwaysOnline || !manifest.Features.Home.Messages || manifest.Features.Home.ReadOnly || !strings.Contains(manifest.Features.Agent.Description, "Organization members") {
			t.Fatal("DM disclosure, presence or Messages tab missing")
		}
		for _, event := range []string{"message.im", "app_home_opened"} {
			if !slices.Contains(manifest.Settings.Events.BotEvents, event) {
				t.Fatal("missing event", event)
			}
		}
		if !slices.Contains(manifest.Settings.Events.BotEvents, "agent_session_stopped") {
			t.Fatal("native Stop subscription missing")
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

func TestSocketManifestHasOnlyBrowserCallbacks(t *testing.T) {
	origin, _ := url.Parse("https://helmr.internal")
	var manifest struct {
		Settings struct {
			Socket        bool                       `json:"socket_mode_enabled"`
			Events        map[string]json.RawMessage `json:"event_subscriptions"`
			Interactivity map[string]json.RawMessage `json:"interactivity"`
		} `json:"settings"`
		OAuth struct {
			Redirects []string `json:"redirect_urls"`
		} `json:"oauth_config"`
	}
	if err := json.Unmarshal(AppManifest(origin, uuid.NewV7(), "Agent", TransportSocket), &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Settings.Socket || len(manifest.OAuth.Redirects) != 2 {
		t.Fatal("missing socket/browser OAuth configuration")
	}
	if _, ok := manifest.Settings.Events["request_url"]; ok {
		t.Fatal("socket app exposes event callback")
	}
	if _, ok := manifest.Settings.Interactivity["request_url"]; ok {
		t.Fatal("socket app exposes interaction callback")
	}
}
