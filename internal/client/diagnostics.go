package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (c *Client) GetCurrentDeployment(ctx context.Context, scope EnvironmentScopeOptions) (api.DeploymentResponse, error) {
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/deployments/current")
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	var result api.DeploymentResponse
	err = c.readDiagnostic(ctx, path, &result)
	return result, err
}

// GetAttention uses the management API, which is available to saved logins only.
func (c *Client) GetAttention(ctx context.Context, scope EnvironmentScopeOptions) (api.Attention, error) {
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/attention")
	if err != nil {
		return api.Attention{}, err
	}
	var result api.Attention
	err = c.readDiagnostic(ctx, path, &result)
	return result, err
}

// SlackConnectionStatus deliberately excludes app credentials and setup manifests.
type SlackConnectionStatus struct {
	ID                    string  `json:"id"`
	Status                string  `json:"status"`
	TeamID                *string `json:"team_id"`
	WorkspaceName         *string `json:"workspace_name"`
	CredentialsConfigured bool    `json:"credentials_configured"`
}

func (c *Client) GetAgentSlackConnection(ctx context.Context, agent string, scope EnvironmentScopeOptions) (*SlackConnectionStatus, error) {
	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/agents/"+url.PathEscape(agent)+"/slack")
	if err != nil {
		return nil, err
	}
	var result struct {
		Connection *SlackConnectionStatus `json:"connection"`
	}
	err = c.readDiagnostic(ctx, path, &result)
	return result.Connection, err
}

type SlackUserLinkStatus struct {
	TeamID string `json:"team_id"`
}

type SlackUserLinksPage struct {
	Links      []SlackUserLinkStatus `json:"links"`
	NextCursor string                `json:"next_cursor"`
}

func (c *Client) GetSlackUserLinks(ctx context.Context, cursor string) (SlackUserLinksPage, error) {
	var result SlackUserLinksPage
	err := c.readDiagnostic(ctx, "/api/slack/user-links?limit=100&cursor="+url.QueryEscape(cursor), &result)
	return result, err
}

func (c *Client) CheckReadiness(ctx context.Context) error {
	var result struct {
		Status string `json:"status"`
	}
	if err := c.readDiagnostic(ctx, "/readyz", &result); err != nil {
		return err
	}
	if result.Status != "ready" {
		return errors.New("server did not report readiness")
	}
	return nil
}

func (c *Client) readDiagnostic(ctx context.Context, path string, out any) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, out)
}

// RetainedAgentsPage includes Agents with retained Sessions or Slack connections,
// even if they are absent from the current deployment.
type RetainedAgentsPage struct {
	Agents     []api.AgentStatus `json:"agents"`
	NextCursor string            `json:"next_cursor"`
}

func (c *Client) ListRetainedAgents(ctx context.Context, scope EnvironmentScopeOptions, cursor string) (RetainedAgentsPage, error) {
	return c.ListAgentCatalog(ctx, scope, cursor, "all", 100)
}
func (c *Client) ListAgentCatalog(ctx context.Context, scope EnvironmentScopeOptions, cursor, view string, limit int32) (RetainedAgentsPage, error) {
	if view != "all" && view != "deleted" {
		return RetainedAgentsPage{}, errors.New("view must be all or deleted")
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return RetainedAgentsPage{}, errors.New("limit must be between 1 and 100")
	}

	path, err := c.environmentScopedPath(scope.ProjectID, scope.EnvironmentID, "/agents")
	if err != nil {
		return RetainedAgentsPage{}, err
	}
	var result RetainedAgentsPage
	query := url.Values{"view": {view}, "limit": {strconv.Itoa(int(limit))}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	err = c.readDiagnostic(ctx, path+"?"+query.Encode(), &result)
	return result, err
}
