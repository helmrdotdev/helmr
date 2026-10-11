package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
)

type AskListOptions struct {
	Cursor string
	Limit  int32
	EnvironmentScopeOptions
}

func (c *Client) askPath(sessionID, turnID, askID string, scope EnvironmentScopeOptions) (string, error) {
	if err := ids.Validate(turnID); err != nil {
		return "", err
	}
	suffix := "/turns/" + url.PathEscape(turnID) + "/asks"
	if askID != "" {
		if err := ids.Validate(askID); err != nil {
			return "", err
		}
		suffix += "/" + url.PathEscape(askID)
	}
	return c.sessionPath(sessionID, suffix, scope)
}

func (c *Client) ListTurnAsks(ctx context.Context, sessionID, turnID string, opts AskListOptions) (api.AgentAsksPage, error) {
	result := api.AgentAsksPage{Asks: []api.AgentAsk{}}
	if opts.Limit < 0 || opts.Limit > 100 {
		return result, errors.New("ask list limit must be in [1,100] when present")
	}
	path, err := c.askPath(sessionID, turnID, "", opts.EnvironmentScopeOptions)
	if err != nil {
		return result, err
	}
	query := url.Values{}
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(int(opts.Limit)))
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(req, &result)
	return result, err
}

func (c *Client) GetTurnAsk(ctx context.Context, sessionID, turnID, askID string, scope EnvironmentScopeOptions) (api.AgentAsk, error) {
	var result api.AgentAsk
	if err := ids.Validate(askID); err != nil {
		return result, err
	}
	path, err := c.askPath(sessionID, turnID, askID, scope)
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

func (c *Client) RespondTurnAsk(ctx context.Context, sessionID, turnID, askID string, input api.RespondAgentAskRequest, scope EnvironmentScopeOptions) (api.AgentAsk, error) {
	var result api.AgentAsk
	if err := ids.Validate(askID); err != nil {
		return result, err
	}
	if !json.Valid(input.Answer) {
		return result, errors.New("answer must be a JSON value")
	}
	path, err := c.askPath(sessionID, turnID, askID, scope)
	if err != nil {
		return result, err
	}
	input.ResponseID = invocationKey(input.ResponseID)
	err = c.postJSON(ctx, path+"/respond", input, &result)
	return result, err
}
