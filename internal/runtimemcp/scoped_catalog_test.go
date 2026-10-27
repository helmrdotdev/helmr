package runtimemcp

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/mcppolicy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestScopedCatalogAndGuidanceUseDescriptorGrants(t *testing.T) {
	for _, raw := range []string{`{"allow":[]}`, `{"allow":[{"operation":"inspect_turn","agents":["helper"]}]}`} {
		t.Run(raw, func(t *testing.T) {
			policy, err := mcppolicy.Normalize(json.RawMessage(raw), []string{"helper"})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(NewScopedHandler(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
				t.Error("local catalog invoked remote operation")
				return nil, nil
			}, policy))
			defer server.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "scoped-test", Version: "1"}, nil)
			session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			catalog, err := session.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, tool := range catalog.Tools {
				names = append(names, tool.Name)
			}
			slices.Sort(names)
			expected := []string{"skills"}
			if policy.Has("inspect_turn") {
				expected = []string{"inspect_turn", "skills", "wait_turn"}
			}
			if !slices.Equal(names, expected) {
				t.Fatalf("catalog %v want %v", names, expected)
			}
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "skills", Arguments: map[string]any{"name": runtimeSkill.Name}})
			if err != nil || result.IsError {
				t.Fatalf("guidance %v %v", result, err)
			}
			body := result.Content[0].(*mcp.TextContent).Text
			for _, denied := range []string{"spawn", "start", "enqueue", "list_agents", "list_computers", "resume_session", "cancel_session"} {
				if strings.Contains(body, denied) {
					t.Fatalf("guidance recommends ungranted %s: %s", denied, body)
				}
			}
		})
	}
}
