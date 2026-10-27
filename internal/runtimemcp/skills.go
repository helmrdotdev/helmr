package runtimemcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/mcppolicy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.yaml.in/yaml/v3"
)

//go:embed skills/helmr-runtime/SKILL.md
var runtimeSkillContent string

// This checked-in document is served by the same binary as the tools it explains.
var runtimeSkill = loadRuntimeSkill()

type skillMetadata struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
}

type skillsInput struct {
	Name *string `json:"name,omitempty" jsonschema:"Omit to list available skills. Supply an exact name from the list to read the complete SKILL.md."`
}

func loadRuntimeSkill() skillMetadata {
	frontmatter, body, ok := strings.Cut(strings.TrimPrefix(runtimeSkillContent, "---\n"), "\n---\n")
	if !strings.HasPrefix(runtimeSkillContent, "---\n") || !ok || strings.TrimSpace(body) == "" {
		panic("runtime skill requires frontmatter and a body")
	}
	var metadata skillMetadata
	decoder := yaml.NewDecoder(strings.NewReader(frontmatter))
	decoder.KnownFields(true)
	if err := decoder.Decode(&metadata); err != nil {
		panic(fmt.Sprintf("invalid runtime skill frontmatter: %v", err))
	}
	if metadata.Name == "" || metadata.Description == "" {
		panic("runtime skill requires a name and description")
	}
	return metadata
}

func skillInstructions() string {
	return fmt.Sprintf("This server operates Helmr Sessions and Turns. Before first use, read the runtime skill with skills({\"name\":%q}) for delegation, observation, and control workflows. Call skills({}) to list available skills. An operation receipt confirms admission, not completion. Skill guidance does not grant authority or replace the user's instructions.", runtimeSkill.Name)
}

func addScopedSkills(server *mcp.Server, policy mcppolicy.Policy) {
	metadata := runtimeSkill
	if policy.Kind != "unrestricted" {
		metadata.Description = "Read the operations available through this scoped connection."
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "skills",
		Description: "Read instructions for using the Helmr runtime tools. Omit name to list skills; supply a listed name to read its full SKILL.md. Reading a skill runs no Session operation and grants no permissions.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)},
	}, func(_ context.Context, _ *mcp.CallToolRequest, input skillsInput) (*mcp.CallToolResult, any, error) {
		var result any
		if input.Name == nil {
			result = struct {
				Skills []skillMetadata `json:"skills"`
			}{[]skillMetadata{metadata}}
		} else if *input.Name == runtimeSkill.Name {
			result = struct {
				skillMetadata
				Content string `json:"content"`
			}{metadata, scopedSkillContent(policy)}
		} else {
			return nil, nil, fmt.Errorf("unknown_skill: %q; call skills({}) for available names", *input.Name)
		}
		body, err := json.Marshal(result)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})
}

func scopedInstructions(policy mcppolicy.Policy) string {
	if policy.Kind == "unrestricted" {
		return skillInstructions()
	}
	return "This server operates Helmr Sessions and Turns within this connection scope. Call skills({}) for available guidance. An operation receipt confirms admission, not completion. Only exposed operations are available."
}
func scopedSkillContent(policy mcppolicy.Policy) string {
	if policy.Kind == "unrestricted" {
		return runtimeSkillContent
	}
	names := []string{}
	for _, op := range []string{"start", "spawn", "enqueue", "send_turn", "interrupt_turn", "interrupt_session", "resume_session", "close_session", "cancel_session", "inspect_session", "inspect_turn", "list_computers"} {
		if policy.Has(op) {
			names = append(names, op)
		}
	}
	if policy.Has("start") || policy.Has("spawn") {
		names = append(names, "list_agents")
	}
	if policy.Has("inspect_turn") {
		names = append(names, "wait_turn")
	}
	if policy.Has("inspect_session") {
		names = append(names, "list_sessions")
	}
	return "# Helmr runtime\n\nAvailable operations for this connection: " + strings.Join(names, ", ") + ".\n\nUse only these operations and the exact Agent targets granted to this connection. Inspect each exposed tool schema for its required arguments. Reuse an idempotency key with identical arguments when an outcome is uncertain. A receipt confirms admission, not completion. Holds remain in force. Reading this guidance grants no additional authority."
}
