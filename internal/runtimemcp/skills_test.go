package runtimemcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSkillsDiscoveryAndRead(t *testing.T) {
	server := httptest.NewServer(NewHandler(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		t.Error("reading a skill invoked a Session operation")
		return nil, nil
	}))
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "skill-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	instructions := session.InitializeResult().Instructions
	if !strings.Contains(instructions, "skills(") || !strings.Contains(instructions, runtimeSkill.Name) {
		t.Fatalf("initialization did not advertise the runtime skill: %s", instructions)
	}
	catalog, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, item := range catalog.Tools {
		if item.Name == "skills" {
			tool = item
		}
	}
	if tool == nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
		t.Fatalf("missing read-only, closed-world skill tool: %+v", tool)
	}
	call := func(input map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "skills", Arguments: input})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	readJSON := func(result *mcp.CallToolResult, target any) {
		t.Helper()
		if result.IsError || len(result.Content) != 1 {
			t.Fatalf("unexpected skill result: %+v", result)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("skill result is not text: %T", result.Content[0])
		}
		if err := json.Unmarshal([]byte(text.Text), target); err != nil {
			t.Fatal(err)
		}
	}
	var listed struct {
		Skills []skillMetadata `json:"skills"`
	}
	readJSON(call(map[string]any{}), &listed)
	if !reflect.DeepEqual(listed.Skills, []skillMetadata{runtimeSkill}) || runtimeSkill.Name != "helmr-runtime" {
		t.Fatalf("unexpected skill metadata: %+v", listed)
	}
	var document struct {
		skillMetadata
		Content string `json:"content"`
	}
	readJSON(call(map[string]any{"name": listed.Skills[0].Name}), &document)
	if document.skillMetadata != listed.Skills[0] || document.Content != runtimeSkillContent {
		t.Fatal("skill metadata or full source changed during delivery")
	}
	// A repeated read is independent of native history and upstream availability.
	var again map[string]any
	readJSON(call(map[string]any{"name": document.Name}), &again)
	if again["content"] != document.Content {
		t.Fatal("repeated read changed the skill")
	}
	for _, name := range []string{"missing", "", "../helmr-runtime/SKILL.md", "https://example.com/SKILL.md"} {
		result := call(map[string]any{"name": name})
		if !result.IsError || len(result.Content) != 1 || !strings.HasPrefix(result.Content[0].(*mcp.TextContent).Text, "unknown_skill:") {
			t.Fatalf("unknown name %q was not rejected: %+v", name, result)
		}
	}
}
