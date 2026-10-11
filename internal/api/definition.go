package api

import (
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/definition"
)

type TemplateDefinition struct {
	Resources    json.RawMessage `json:"resources"`
	ID           string          `json:"id"`
	DeploymentID string          `json:"deployment_id"`
}
type ListTemplateDefinitionsResponse struct {
	DeploymentID        string               `json:"deployment_id"`
	TemplateDefinitions []DefinitionListItem `json:"template_definitions"`
	NextCursor          string               `json:"next_cursor,omitempty"`
}

func ValidateDefinitionID(id string) error {
	if !definition.ValidDeclaredID(id) {
		return fmt.Errorf("declared ID %q must match %s", id, definition.DeclaredIDGrammar)
	}
	return nil
}

type DefinitionListItem struct {
	Resources json.RawMessage `json:"resources"`
	ID        string          `json:"id"`
}
