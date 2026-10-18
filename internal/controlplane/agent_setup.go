package controlplane

import (
	"bytes"
	_ "embed"
	"net/http"
	"text/template"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/helmrdotdev/helmr/starters"
)

//go:embed agent_setup.md
var agentSetupSource string

var agentSetupTemplate = template.Must(template.New("agent-setup").Parse(agentSetupSource))

func (s *Server) agentSetup(w http.ResponseWriter, r *http.Request) {
	starter, ok := starters.Lookup(chi.URLParam(r, "harness"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	binding, err := starter.Binding(r.URL.Query().Get("provider"))
	if err != nil {
		http.Error(w, "unsupported provider for this harness", http.StatusBadRequest)
		return
	}
	var body bytes.Buffer
	if err := agentSetupTemplate.Execute(&body, struct {
		Harness string
		Binding starters.Binding
		Node    string
	}{starter.ID, binding, version.Node()}); err != nil {
		http.Error(w, "could not render setup instructions", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body.Bytes())
}
