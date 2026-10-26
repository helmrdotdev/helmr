package api

import (
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/definition"
)

type TemplateDefinition struct {
	Network        json.RawMessage `json:"network"`
	PrepareNetwork json.RawMessage `json:"prepare_network"`
	PrepareSecrets json.RawMessage `json:"prepare_secrets"`
	Resources      json.RawMessage `json:"resources"`
	ID             string          `json:"template"`
	DeploymentID   string          `json:"deployment_id"`
}
type ListTemplateDefinitionsResponse struct {
	DeploymentID        string               `json:"deployment_id"`
	TemplateDefinitions []DefinitionListItem `json:"templates"`
	NextCursor          string               `json:"next_cursor,omitempty"`
}

func ValidateDefinitionID(id string) error {
	if !definition.ValidDeclaredID(id) {
		return fmt.Errorf("declared ID %q must match %s", id, definition.DeclaredIDGrammar)
	}
	return nil
}

type DefinitionListItem struct {
	Network        json.RawMessage `json:"network"`
	PrepareNetwork json.RawMessage `json:"prepare_network"`
	PrepareSecrets json.RawMessage `json:"prepare_secrets"`
	Resources      json.RawMessage `json:"resources"`
	ID             string          `json:"template"`
}
