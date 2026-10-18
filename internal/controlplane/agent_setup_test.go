package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/starters"
)

func TestAgentSetupVariants(t *testing.T) {
	router := chi.NewRouter()
	(&Server{}).mountRoutes(router)
	count := 0
	for _, id := range starters.IDs() {
		starter, _ := starters.Lookup(id)
		for _, provider := range starter.ProviderIDs() {
			count++
			t.Run(id+"/"+provider, func(t *testing.T) {
				binding, _ := starter.Binding(provider)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/agent-setup/"+id+".md?provider="+provider, nil))
				if response.Code != http.StatusOK {
					t.Fatalf("status %d: %s", response.Code, response.Body.String())
				}
				if got := response.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
					t.Fatalf("content type %q", got)
				}
				for _, want := range []string{
					"helmr init --template " + id + " --provider " + provider,
					binding.Model, binding.Provider.Secret, binding.Provider.Origin, binding.Env,
					"helmr login <server>", "helmr deploy --api-url <server> --project <project> --env <environment>",
					"Never ask for, read or print", "HAND-OFF", "first Slack reply", "Agent:", "--name", "--force",
					"/settings/secrets?project=<project>&environment=<environment>",
					"/agents/<agent>/slack?project=<project>&environment=<environment>",
					"then mentions the\nAgent again", "helmr doctor", "HELMR_API_KEY",
				} {
					if !strings.Contains(response.Body.String(), want) {
						t.Errorf("missing %q", want)
					}
				}
				if strings.Contains(response.Body.String(), "{{") {
					t.Error("unrendered template")
				}
			})
		}
	}
	if count != 8 {
		t.Fatalf("variant count %d, want 8", count)
	}
}

func TestAgentSetupSelection(t *testing.T) {
	router := chi.NewRouter()
	(&Server{}).mountRoutes(router)
	for _, test := range []struct {
		path     string
		status   int
		contains string
	}{
		{"/agent-setup/unknown.md", 404, ""},
		{"/agent-setup/codex.md?provider=anthropic", 400, ""},
		{"/agent-setup/pi.md?provider=unknown", 400, ""},
		{"/agent-setup/opencode.md", 200, "--provider anthropic"},
		{"/agent-setup/codex.md", 200, "--provider openai"},
		{"/agent-setup/pi.md?project=UNTRUSTED&environment=UNTRUSTED", 200, "--provider anthropic"},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != test.status {
				t.Fatalf("status %d, want %d", response.Code, test.status)
			}
			if !strings.Contains(response.Body.String(), test.contains) {
				t.Fatalf("missing %q", test.contains)
			}
			if strings.Contains(response.Body.String(), "UNTRUSTED") {
				t.Error("request input interpolated into instructions")
			}
		})
	}
}

func TestAgentSetupPublicServerRoute(t *testing.T) {
	handler, err := NewServer(completeServerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/agent-setup/codex.md?provider=openai", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "helmr init --template codex") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}
