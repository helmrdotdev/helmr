package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func (c *Client) BaseURL() string {
	return c.transport.BaseURL()
}

func (c *Client) GetOrganizationContext(ctx context.Context) (api.OrganizationContextResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/organization", nil)
	if err != nil {
		return api.OrganizationContextResponse{}, err
	}
	var response api.OrganizationContextResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.OrganizationContextResponse{}, err
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
	input api.CreateComputerRequest,
	opts ComputerScopeOptions,
) (api.ComputerSnapshot, error) {
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/computers")
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

type ComputerListOptions struct {
	Key *string
	// Secret lists only Computers that received a raw value of this Secret,
	// directly or through their image.
	Secret string
	Cursor string
	Limit  int32
}

func (c *Client) ListComputers(
	ctx context.Context,
	query ComputerListOptions,
	opts ComputerScopeOptions,
) (api.ListComputersResponse, error) {
	path, err := c.computerCollectionPath(opts)
	if err != nil {
		return api.ListComputersResponse{}, err
	}
	if query.Limit < 0 || query.Limit > 100 {
		return api.ListComputersResponse{}, errors.New("limit must be in [1,100]")
	}
	if query.Key != nil && (query.Cursor != "" || query.Limit != 0 || query.Secret != "") {
		return api.ListComputersResponse{}, errors.New("key lookup does not accept secret, cursor or limit")
	}
	values := url.Values{}
	if query.Key != nil {
		values.Set("key", *query.Key)
	}
	if query.Secret != "" {
		values.Set("secret", query.Secret)
	}
	if query.Cursor != "" {
		values.Set("cursor", query.Cursor)
	}
	if query.Limit > 0 {
		values.Set("limit", strconv.Itoa(int(query.Limit)))
	}
	if len(values) > 0 {
		path += "?" + values.Encode()
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
	dryRun bool,
	opts ComputerScopeOptions,
) (api.DeleteComputerReceipt, error) {
	path, err := c.computerResourcePath(computerID, "", opts)
	if err != nil {
		return api.DeleteComputerReceipt{}, err
	}
	if dryRun {
		path += "?dry_run=true"
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
	input.IdempotencyKey = invocationKey(input.IdempotencyKey)
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

func (c *Client) ListCommandLogs(ctx context.Context, id, cursor string, limit int, opts ComputerScopeOptions) (api.CommandLogPage, error) {
	if err := ids.Validate(id); err != nil {
		return api.CommandLogPage{}, err
	}
	if limit < 0 || limit > 100 {
		return api.CommandLogPage{}, errors.New("log limit must be in [1,100]")
	}
	path, err := c.environmentScopedPath(opts.ProjectID, opts.EnvironmentID, "/commands/"+url.PathEscape(id)+"/logs")
	if err != nil {
		return api.CommandLogPage{}, err
	}
	query := url.Values{}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if limit > 0 {
		query.Set("limit", fmt.Sprint(limit))
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	request, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.CommandLogPage{}, err
	}
	var result api.CommandLogPage
	if err := c.doJSON(request, &result); err != nil {
		return api.CommandLogPage{}, err
	}
	return result, nil
}

type ComputerUsageOptions struct {
	Cursor string
	Limit  int32
}

func (c *Client) computerUsagePath(computerID, kind string, query ComputerUsageOptions, opts ComputerScopeOptions) (string, error) {
	if query.Limit < 0 || query.Limit > 100 {
		return "", errors.New("limit must be in [1,100]")
	}
	path, err := c.computerResourcePath(computerID, "/"+kind, opts)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	if query.Cursor != "" {
		values.Set("cursor", query.Cursor)
	}
	if query.Limit > 0 {
		values.Set("limit", strconv.Itoa(int(query.Limit)))
	}
	if len(values) > 0 {
		path += "?" + values.Encode()
	}
	return path, nil
}
func (c *Client) ListComputerSessions(ctx context.Context, computerID string, query ComputerUsageOptions, opts ComputerScopeOptions) (api.ListComputerSessionsResponse, error) {
	var result api.ListComputerSessionsResponse
	path, err := c.computerUsagePath(computerID, "sessions", query, opts)
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
func (c *Client) ListComputerCommands(ctx context.Context, computerID string, query ComputerUsageOptions, opts ComputerScopeOptions) (api.ListComputerCommandsResponse, error) {
	var result api.ListComputerCommandsResponse
	path, err := c.computerUsagePath(computerID, "commands", query, opts)
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
