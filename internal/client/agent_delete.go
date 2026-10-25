package client

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/api"
	"net/http"
	"net/url"
)

func (c *Client) GetAgentStatus(ctx context.Context, agent string, scope EnvironmentScopeOptions) (api.AgentStatus, error) {
	var out api.AgentStatus
	if err := api.ValidateDefinitionID(agent); err != nil {
		return out, err
	}
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/agents/"+url.PathEscape(agent)+"/status")
	if err != nil {
		return out, err
	}
	err = c.permissionRequest(ctx, http.MethodGet, path, nil, &out, "")
	return out, err
}
func (c *Client) ChangeAgent(ctx context.Context, agent string, scope EnvironmentScopeOptions, restore, dryRun bool, revision string) (api.AgentChangeResponse, error) {
	var out api.AgentChangeResponse
	if err := api.ValidateDefinitionID(agent); err != nil {
		return out, err
	}
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/agents/"+url.PathEscape(agent))
	if err != nil {
		return out, err
	}
	method := http.MethodDelete
	var input any
	if restore {
		method = http.MethodPost
		path += "/restore"
		input = struct {
			DryRun bool `json:"dry_run"`
		}{dryRun}
	} else if dryRun {
		path += "?dry_run=true"
	}
	err = c.permissionRequest(ctx, method, path, input, &out, revision)
	return out, err
}
