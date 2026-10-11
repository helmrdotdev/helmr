package api

import (
	"fmt"
	"github.com/helmrdotdev/helmr/internal/definition"
)

type ComputerDefinition struct {
	ID           string `json:"id"`
	DeploymentID string `json:"deployment_id"`
}
type ListComputerDefinitionsResponse struct {
	DeploymentID        string               `json:"deployment_id"`
	ComputerDefinitions []DefinitionListItem `json:"computer_definitions"`
	NextCursor          string               `json:"next_cursor,omitempty"`
}

func ValidateDefinitionID(id string) error {
	if !definition.ValidDeclaredID(id) {
		return fmt.Errorf("declared ID %q must match %s", id, definition.DeclaredIDGrammar)
	}
	return nil
}

type DefinitionListItem struct {
	ID string `json:"id"`
}
