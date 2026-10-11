package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func (c *Client) BaseURL() string {
	return c.transport.BaseURL()
}

func (c *Client) GetMe(ctx context.Context) (api.MeResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/me", nil)
	if err != nil {
		return api.MeResponse{}, err
	}
	var response api.MeResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.MeResponse{}, err
	}
	return response, nil
}

type EnvironmentScopeOptions struct {
	ProjectID     string
	EnvironmentID string
}

func (c *Client) ListDeployments(ctx context.Context, opts EnvironmentScopeOptions) (api.ListDeploymentsResponse, error) {
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/deployments")
	if err != nil {
		return api.ListDeploymentsResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListDeploymentsResponse{}, err
	}
	var response api.ListDeploymentsResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListDeploymentsResponse{}, err
	}
	return response, nil
}

type ComputerScopeOptions struct {
	ProjectID     string
	EnvironmentID string
}

func (c *Client) computerCollectionPath(opts ComputerScopeOptions) (string, error) {
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/computers")
	return path, err
}

func (c *Client) computerItemPath(computerID string, suffix string, opts ComputerScopeOptions) (string, error) {
	path, err := c.computerCollectionPath(opts)
	if err != nil {
		return "", err
	}
	return environmentScopedResourcePath(path, computerID, suffix), nil
}

func (c *Client) computerResourcePath(computerID string, suffix string, opts ComputerScopeOptions) (string, error) {
	if err := ids.Validate(computerID); err != nil {
		return "", err
	}
	return c.computerItemPath(computerID, suffix, opts)
}

func (c *Client) CreateComputer(
	ctx context.Context,
	declaredID string,
	input api.CreateComputerRequest,
	opts ComputerScopeOptions,
) (api.ComputerSnapshot, error) {
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/computer-definitions/"+url.PathEscape(declaredID)+"/computers")
	if err != nil {
		return api.ComputerSnapshot{}, err
	}
	var response api.ComputerSnapshot
	if err := c.postJSON(ctx, path, input, &response); err != nil {
		return api.ComputerSnapshot{}, err
	}
	return response, nil
}

func (c *Client) GetComputer(ctx context.Context, computerID string, opts ComputerScopeOptions) (api.ComputerSnapshot, error) {
	path, err := c.computerResourcePath(computerID, "", opts)
	if err != nil {
		return api.ComputerSnapshot{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ComputerSnapshot{}, err
	}
	var response api.ComputerSnapshot
	if err := c.doJSON(req, &response); err != nil {
		return api.ComputerSnapshot{}, err
	}
	return response, nil
}

func (c *Client) ListComputers(
	ctx context.Context,
	key *string,
	opts ComputerScopeOptions,
) (api.ListComputersResponse, error) {
	path, err := c.computerCollectionPath(opts)
	if err != nil {
		return api.ListComputersResponse{}, err
	}
	if key != nil {
		path += "?" + url.Values{"key": []string{*key}}.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListComputersResponse{}, err
	}
	var response api.ListComputersResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListComputersResponse{}, err
	}
	return response, nil
}

func (c *Client) DeleteComputer(
	ctx context.Context,
	computerID string,
	input api.DeleteComputerRequest,
	opts ComputerScopeOptions,
) (api.DeleteComputerReceipt, error) {
	path, err := c.computerResourcePath(computerID, "", opts)
	if err != nil {
		return api.DeleteComputerReceipt{}, err
	}
	var response api.DeleteComputerReceipt
	if err := c.deleteJSON(ctx, path, input, &response); err != nil {
		return api.DeleteComputerReceipt{}, err
	}
	return response, nil
}

func (c *Client) ExecuteComputer(ctx context.Context, computerID string, input api.ExecuteComputerRequest, opts ComputerScopeOptions) (api.CommandReceipt, error) {
	path, err := c.computerResourcePath(computerID, "/exec", opts)
	if err != nil {
		return api.CommandReceipt{}, err
	}
	var receipt api.CommandReceipt
	if err := c.postJSON(ctx, path, input, &receipt); err != nil {
		return api.CommandReceipt{}, err
	}
	if err := ids.Validate(receipt.CommandID); err != nil {
		return api.CommandReceipt{}, fmt.Errorf("invalid command ID: %w", err)
	}
	return receipt, nil
}

func (c *Client) RetrieveCommand(ctx context.Context, id string, opts ComputerScopeOptions) (api.CommandInfo, error) {
	if err := ids.Validate(id); err != nil {
		return api.CommandInfo{}, err
	}
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/commands/"+url.PathEscape(id))
	if err != nil {
		return api.CommandInfo{}, err
	}
	request, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.CommandInfo{}, err
	}
	var result api.CommandInfo
	if err := c.doJSON(request, &result); err != nil {
		return api.CommandInfo{}, err
	}
	if result.ID != id {
		return api.CommandInfo{}, errors.New("exec response changed ID")
	}
	return result, nil
}

func (c *Client) CancelCommand(ctx context.Context, id string, opts ComputerScopeOptions) (api.CommandCancelReceipt, error) {
	if err := ids.Validate(id); err != nil {
		return api.CommandCancelReceipt{}, err
	}
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/commands/"+url.PathEscape(id)+"/cancel")
	if err != nil {
		return api.CommandCancelReceipt{}, err
	}
	var receipt api.CommandCancelReceipt
	if err := c.postJSON(ctx, path, nil, &receipt); err != nil {
		return api.CommandCancelReceipt{}, err
	}
	if err := ids.Validate(receipt.ID); err != nil {
		return api.CommandCancelReceipt{}, err
	}
	if receipt.TargetID != id || receipt.Status != "accepted" {
		return api.CommandCancelReceipt{}, errors.New("invalid Command cancellation receipt")
	}
	return receipt, nil
}
