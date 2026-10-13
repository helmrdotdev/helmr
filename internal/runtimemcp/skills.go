package runtimemcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

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

func addSkills(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "skills",
		Description: "Read instructions for using the Helmr runtime tools. Omit name to list skills; supply a listed name to read its full SKILL.md. Reading a skill runs no Session operation and grants no permissions.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)},
	}, func(_ context.Context, _ *mcp.CallToolRequest, input skillsInput) (*mcp.CallToolResult, any, error) {
		var result any
		if input.Name == nil {
			result = struct {
				Skills []skillMetadata `json:"skills"`
			}{[]skillMetadata{runtimeSkill}}
		} else if *input.Name == runtimeSkill.Name {
			result = struct {
				skillMetadata
				Content string `json:"content"`
			}{runtimeSkill, runtimeSkillContent}
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
