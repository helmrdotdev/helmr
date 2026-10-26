// Package starters holds the Agent projects written by `helmr init --template`.
//
// Each starter is a real project in this directory. scripts/check-starters.sh
// typechecks and tests it against the local SDK. `helmr init` copies its files, writes
// agents/settings.ts for the chosen name and model provider, and writes a
// package.json that depends on the released SDK.
package starters

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

//go:embed claude-code/.helmrignore claude-code/README.md claude-code/helmr.config.ts claude-code/package.json claude-code/tsconfig.json claude-code/agents
//go:embed codex/.helmrignore codex/README.md codex/helmr.config.ts codex/package.json codex/tsconfig.json codex/agents
//go:embed opencode/.helmrignore opencode/README.md opencode/helmr.config.ts opencode/package.json opencode/tsconfig.json opencode/agents
//go:embed pi/.helmrignore pi/README.md pi/helmr.config.ts pi/package.json pi/tsconfig.json pi/agents
var files embed.FS

const (
	// AgentFile is the starter's Agent definition; init renames it after the Agent.
	AgentFile = "agents/agent.ts"
	// SettingsFile holds the Agent name, introduction, model and model key binding.
	SettingsFile = "agents/settings.ts"
	// PackageFile is rewritten to depend on the released SDK.
	PackageFile = "package.json"
)

// Provider is a model provider and the Secret that holds its API key.
type Provider struct {
	ID     string
	Secret string
	Origin string
}

var providers = map[string]Provider{
	"opencode-go": {ID: "opencode-go", Secret: "OPENCODE_GO_API_KEY", Origin: "https://opencode.ai"},
	"openai":      {ID: "openai", Secret: "OPENAI_API_KEY", Origin: "https://api.openai.com"},
	"anthropic":   {ID: "anthropic", Secret: "ANTHROPIC_API_KEY", Origin: "https://api.anthropic.com"},
}

// Binding is how a starter uses one provider: the model and the env var that
// carries the key inside the Computer.
type Binding struct {
	Provider   Provider
	Model      string
	SmallModel string
	Env        string
}

// Starter is one harness template.
type Starter struct {
	ID       string
	bindings []Binding
}

var all = []Starter{
	{ID: "codex", bindings: []Binding{
		{Provider: providers["openai"], Model: "gpt-6.1-sol", Env: "CODEX_API_KEY"},
	}},
	{ID: "claude-code", bindings: []Binding{
		{Provider: providers["anthropic"], Model: "claude-opus-5-5", Env: "ANTHROPIC_API_KEY"},
	}},
	{ID: "opencode", bindings: []Binding{
		{Provider: providers["anthropic"], Model: "anthropic/claude-opus-5-5", SmallModel: "anthropic/claude-haiku-5-5", Env: "ANTHROPIC_API_KEY"},
		{Provider: providers["openai"], Model: "openai/gpt-6.1-sol", SmallModel: "openai/gpt-6-luna", Env: "OPENAI_API_KEY"},
		{Provider: providers["opencode-go"], Model: "opencode-go/glm-5.3", SmallModel: "opencode-go/glm-5.3", Env: "OPENCODE_API_KEY"},
	}},
	{ID: "pi", bindings: []Binding{
		{Provider: providers["anthropic"], Model: "anthropic/claude-opus-5-5", Env: "ANTHROPIC_API_KEY"},
		{Provider: providers["openai"], Model: "openai/gpt-6.1-sol", Env: "OPENAI_API_KEY"},
		{Provider: providers["opencode-go"], Model: "opencode-go/glm-5.3", Env: "OPENCODE_API_KEY"},
	}},
}

// IDs lists the starter IDs in display order.
func IDs() []string {
	ids := make([]string, len(all))
	for i, starter := range all {
		ids[i] = starter.ID
	}
	return ids
}

// Lookup returns the starter with the given ID.
func Lookup(id string) (Starter, bool) {
	for _, starter := range all {
		if starter.ID == id {
			return starter, true
		}
	}
	return Starter{}, false
}

// Binding returns the binding for a provider ID; an empty ID selects the default.
func (s Starter) Binding(provider string) (Binding, error) {
	if provider == "" {
		return s.bindings[0], nil
	}
	for _, binding := range s.bindings {
		if binding.Provider.ID == provider {
			return binding, nil
		}
	}
	return Binding{}, fmt.Errorf("template %s does not support provider %q (supported: %s)", s.ID, provider, strings.Join(s.ProviderIDs(), ", "))
}

// ProviderIDs lists the providers this starter supports, default first.
func (s Starter) ProviderIDs() []string {
	ids := make([]string, len(s.bindings))
	for i, binding := range s.bindings {
		ids[i] = binding.Provider.ID
	}
	return ids
}

// Files returns the starter's project files rooted at the project directory.
func (s Starter) Files() fs.FS {
	sub, err := fs.Sub(files, s.ID)
	if err != nil {
		panic(err)
	}
	return sub
}

// Settings renders agents/settings.ts.
func Settings(name string, binding Binding) string {
	introduction := "You are " + name + ", a Helmr agent. When someone greets you or asks who you are, introduce yourself in one short sentence."
	if namesake, ok := LookupNamesake(name); ok {
		introduction = "You are " + name + ", a Helmr agent named after the " + namesake.Field + " " + namesake.Person +
			". When someone greets you or asks who you are, introduce yourself in one short sentence that mentions your namesake."
	}
	var b strings.Builder
	b.WriteString("// Written by `helmr init`. Change the model or the key binding here.\n")
	b.WriteString("export const agentName = " + strconv.Quote(name) + "\n")
	b.WriteString("export const introduction = " + strconv.Quote(introduction) + "\n")
	b.WriteString("export const model = " + strconv.Quote(binding.Model) + "\n")
	if binding.SmallModel != "" {
		b.WriteString("export const smallModel = " + strconv.Quote(binding.SmallModel) + "\n")
	}
	b.WriteString("export const modelKey = { secret: " + strconv.Quote(binding.Provider.Secret) +
		", env: " + strconv.Quote(binding.Env) + ", origin: " + strconv.Quote(binding.Provider.Origin) + " } as const\n")
	return b.String()
}

type sourcePackage struct {
	PackageManager  string            `json:"packageManager"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Overrides       map[string]string `json:"overrides"`
}

type devEngines struct {
	Runtime struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		OnFail  string `json:"onFail"`
	} `json:"runtime"`
}

type generatedPackage struct {
	Private         bool              `json:"private"`
	Type            string            `json:"type"`
	PackageManager  string            `json:"packageManager"`
	DevEngines      devEngines        `json:"devEngines"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies,omitempty"`
	Overrides       map[string]string `json:"overrides,omitempty"`
}

// PackageJSON renders the project's package.json with the released SDK
// version and the starter's own dependencies.
func (s Starter) PackageJSON(sdkVersion, nodeVersion string) (string, error) {
	raw, err := fs.ReadFile(s.Files(), PackageFile)
	if err != nil {
		return "", err
	}
	var source sourcePackage
	if err := json.Unmarshal(raw, &source); err != nil {
		return "", err
	}
	if _, ok := source.Dependencies["@helmr/sdk"]; !ok {
		return "", fmt.Errorf("template %s does not depend on @helmr/sdk", s.ID)
	}
	out := generatedPackage{
		Private:         true,
		Type:            "module",
		PackageManager:  source.PackageManager,
		Scripts:         map[string]string{"typecheck": "tsc --noEmit"},
		Dependencies:    map[string]string{},
		DevDependencies: source.DevDependencies,
	}
	out.DevEngines.Runtime.Name = "node"
	out.DevEngines.Runtime.Version = nodeVersion
	out.DevEngines.Runtime.OnFail = "error"
	for name, version := range source.Dependencies {
		if name == "@helmr/sdk" {
			version = sdkVersion
		}
		out.Dependencies[name] = version
	}
	for name, version := range source.Overrides {
		// The local proto package is only for repository verification.
		if name == "@helmr/proto" {
			continue
		}
		if out.Overrides == nil {
			out.Overrides = make(map[string]string)
		}
		out.Overrides[name] = version
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(out); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ProjectFiles lists the files init writes for this starter, as project
// paths, with the Agent file renamed after the Agent.
func (s Starter) ProjectFiles(name string) ([]string, error) {
	var paths []string
	err := fs.WalkDir(s.Files(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		switch path {
		case AgentFile:
			path = AgentPath(name)
		case AgentPath(name):
			return fmt.Errorf("agent name %q collides with %s in template %s", name, path, s.ID)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	return paths, nil
}

// AgentPath is the project path of the Agent definition for a name.
func AgentPath(name string) string {
	return "agents/" + name + ".ts"
}
