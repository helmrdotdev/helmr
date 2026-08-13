package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/clistate"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/spf13/cobra"
)

func TestRootCommandPrintsVersion(t *testing.T) {
	const testVersion = "v0.0.0-test"
	originalVersion := version.Version
	version.Version = testVersion
	t.Cleanup(func() {
		version.Version = originalVersion
	})

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != testVersion {
		t.Fatalf("version output = %q", out.String())
	}
}

func installTestCLIConfig(t *testing.T) *testCLIState {
	t.Helper()
	state := &testCLIState{tokens: map[string]string{}}
	previous := newCLIStateStore
	newCLIStateStore = func() (cliState, error) {
		return state, nil
	}
	t.Cleanup(func() {
		newCLIStateStore = previous
	})
	return state
}

type testCLIState struct {
	config clistate.Config
	tokens map[string]string
}

func (s *testCLIState) Load() (clistate.Config, error) {
	if s.config.DefaultHost == "" {
		return clistate.Config{}, clistate.ErrNotFound
	}
	return s.config, nil
}

func (s *testCLIState) Token(baseURL string) (string, error) {
	token, ok := s.tokens[baseURL]
	if !ok {
		return "", clistate.ErrNotFound
	}
	return token, nil
}

func (s *testCLIState) SaveLogin(baseURL, token string) error {
	s.config.DefaultHost = baseURL
	s.tokens[baseURL] = token
	return nil
}

func (s *testCLIState) DeleteToken(baseURL string) error {
	delete(s.tokens, baseURL)
	return nil
}

func TestCommandSurface(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{
		{"build"},
		{"workspace"},
		{"task"},
		{"actor"},
		{"run"},
		{"schedule"},
		{"deployment"},
		{"token"},
		{"task", "start"},
		{"actor", "start"},
		{"actor", "get"},
		{"actor", "input", "send"},
		{"actor", "output", "read"},
		{"actor", "close"},
		{"schedule", "list"},
		{"schedule", "get"},
		{"token", "get"},
		{"token", "complete"},
		{"token", "cancel"},
	} {
		if commandByPath(root, path...) == nil {
			t.Fatalf("command %q is not registered", strings.Join(path, " "))
		}
	}
}

func commandByPath(root *cobra.Command, path ...string) *cobra.Command {
	current := root
	for _, name := range path {
		found := false
		for _, child := range current.Commands() {
			if child.Name() == name {
				current = child
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return current
}
