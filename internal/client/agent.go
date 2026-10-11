package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/definition"
)

func (c *Client) StartAgent(ctx context.Context, name string, input api.StartAgentRequest, scope EnvironmentScopeOptions) (api.StartAgentResponse, error) {
	var result api.StartAgentResponse
	if !definition.ValidDeclaredID(name) {
		return result, fmt.Errorf("invalid Agent name")
	}
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/agents/"+url.PathEscape(name)+"/start")
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return result, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(raw))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	err = c.doJSON(req, &result)
	return result, err
}
