package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/definition"
)

func (c *Client) CreateSession(ctx context.Context, input api.CreateSessionRequest, scope EnvironmentScopeOptions) (api.CreateSessionResponse, error) {
	var result api.CreateSessionResponse
	if !definition.ValidDeclaredID(input.Agent) {
		return result, fmt.Errorf("invalid Agent name")
	}
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/sessions")
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
