package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
)

const maxTriggerListLimit = int32(100)

type ListTriggersOptions struct {
	View   string
	Key    string
	Agent  string
	Cursor string
	Limit  int32
	EnvironmentScopeOptions
}

func (c *Client) ListTriggers(
	ctx context.Context,
	opts ListTriggersOptions,
) (api.ListTriggersResponse, error) {
	if opts.Limit < 0 || opts.Limit > maxTriggerListLimit {
		return api.ListTriggersResponse{}, fmt.Errorf(
			"trigger list limit must be in [1,%d] when present",
			maxTriggerListLimit,
		)
	}
	path, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/triggers",
	)
	if err != nil {
		return api.ListTriggersResponse{}, err
	}
	query := url.Values{}
	if opts.View != "" {
		if opts.View != "current" && opts.View != "all" {
			return api.ListTriggersResponse{}, fmt.Errorf("view must be current or all")
		}
		query.Set("view", opts.View)
	}
	if opts.Key != "" {
		query.Set("key", opts.Key)
	}
	if opts.Agent != "" {
		if err := api.ValidateDefinitionID(opts.Agent); err != nil {
			return api.ListTriggersResponse{}, err
		}
		query.Set("agent", opts.Agent)
	}

	if cursor := strings.TrimSpace(opts.Cursor); cursor != "" {
		query.Set("cursor", cursor)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.FormatInt(int64(opts.Limit), 10))
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.ListTriggersResponse{}, err
	}
	var response api.ListTriggersResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.ListTriggersResponse{}, err
	}
	if response.Triggers == nil {
		response.Triggers = []api.TriggerResponse{}
	}
	return response, nil
}

func (c *Client) GetTrigger(
	ctx context.Context,
	triggerID string,
	opts EnvironmentScopeOptions,
) (api.TriggerResponse, error) {
	if err := api.ValidateTriggerID(triggerID); err != nil {
		return api.TriggerResponse{}, err
	}
	path, err := c.environmentScopedPath(
		opts.ProjectID,
		opts.EnvironmentID,
		"/triggers/"+url.PathEscape(triggerID),
	)
	if err != nil {
		return api.TriggerResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return api.TriggerResponse{}, err
	}
	var response api.TriggerResponse
	if err := c.doJSON(req, &response); err != nil {
		return api.TriggerResponse{}, err
	}
	return response, nil
}

func (c *Client) ListTriggerOccurrences(ctx context.Context, id string, scope EnvironmentScopeOptions, cursor string) (api.TriggerOccurrencesPage, error) {
	var out api.TriggerOccurrencesPage
	if err := api.ValidateTriggerID(id); err != nil {
		return out, err
	}
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/triggers/"+url.PathEscape(id)+"/occurrences")
	if err != nil {
		return out, err
	}
	if cursor != "" {
		path += "?cursor=" + url.QueryEscape(cursor)
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return out, err
	}
	err = c.doJSON(req, &out)
	return out, err
}
