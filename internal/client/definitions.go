package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
)

type DefinitionListOptions struct {
	DeploymentID string
	Cursor       string
	Limit        int32
	EnvironmentScopeOptions
}

type DefinitionGetOptions struct {
	DeploymentID string
	EnvironmentScopeOptions
}

func (c *Client) definitionListPath(collection string, opts DefinitionListOptions) (string, error) {
	if opts.Limit < 0 || opts.Limit > 100 {
		return "", errors.New("definition list limit must be in [1,100] when present")
	}
	path, err := c.definitionPath(collection, "", DefinitionGetOptions{DeploymentID: opts.DeploymentID, EnvironmentScopeOptions: opts.EnvironmentScopeOptions})
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(int(opts.Limit)))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (c *Client) definitionPath(collection, id string, opts DefinitionGetOptions) (string, error) {
	if id != "" {
		if err := api.ValidateDefinitionID(id); err != nil {
			return "", err
		}
		collection += "/" + url.PathEscape(id)
	}
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, collection)
	if err != nil {
		return "", err
	}
	if opts.DeploymentID != "" {
		if err := ids.Validate(opts.DeploymentID); err != nil {
			return "", err
		}
		path += "?" + url.Values{"deployment_id": []string{opts.DeploymentID}}.Encode()
	}
	return path, nil
}

func (c *Client) ListAgents(ctx context.Context, opts DefinitionListOptions) (api.ListAgentsResponse, error) {
	result := api.ListAgentsResponse{Agents: []api.DefinitionListItem{}}
	path, err := c.definitionListPath("/agents", opts)
	if err != nil {
		return result, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(req, &result)
	return result, err
}

func (c *Client) GetAgent(ctx context.Context, id string, opts DefinitionGetOptions) (api.AgentDefinition, error) {
	var result api.AgentDefinition
	if err := api.ValidateDefinitionID(id); err != nil {
		return result, err
	}
	path, err := c.definitionPath("/agents", id, opts)
	if err != nil {
		return result, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(req, &result)
	return result, err
}

func (c *Client) ListComputerDefinitions(ctx context.Context, opts DefinitionListOptions) (api.ListComputerDefinitionsResponse, error) {
	result := api.ListComputerDefinitionsResponse{ComputerDefinitions: []api.DefinitionListItem{}}
	path, err := c.definitionListPath("/computer-definitions", opts)
	if err != nil {
		return result, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(req, &result)
	return result, err
}

func (c *Client) GetComputerDefinition(ctx context.Context, id string, opts DefinitionGetOptions) (api.ComputerDefinition, error) {
	var result api.ComputerDefinition
	if err := api.ValidateDefinitionID(id); err != nil {
		return result, err
	}
	path, err := c.definitionPath("/computer-definitions", id, opts)
	if err != nil {
		return result, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(req, &result)
	return result, err
}
