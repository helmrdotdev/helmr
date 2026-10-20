package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/ids"
)

func (c *Client) RefreshTemplate(ctx context.Context, template, key string, scope EnvironmentScopeOptions) (api.TemplateBuildResponse, error) {
	var result api.TemplateBuildResponse
	path, err := c.definitionPath("/templates", template, DefinitionGetOptions{EnvironmentScopeOptions: scope})
	if err != nil {
		return result, err
	}
	err = c.postJSON(ctx, path+"/refresh", api.RefreshTemplateRequest{IdempotencyKey: invocationKey(key)}, &result)
	return result, err
}

func (c *Client) GetTemplateBuild(ctx context.Context, template, build, deployment string, scope EnvironmentScopeOptions) (api.TemplateBuildResponse, error) {
	var result api.TemplateBuildResponse
	if err := ids.Validate(build); err != nil {
		return result, err
	}
	if err := ids.Validate(deployment); err != nil {
		return result, err
	}
	path, err := c.definitionPath("/templates", template, DefinitionGetOptions{EnvironmentScopeOptions: scope})
	if err != nil {
		return result, err
	}
	path += "/builds/" + url.PathEscape(build) + "?" + url.Values{"deployment_id": {deployment}}.Encode()
	request, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return result, err
	}
	err = c.doJSON(request, &result)
	return result, err
}

// WaitTemplateBuild follows one immutable build; it never retries preparation.
func (c *Client) WaitTemplateBuild(ctx context.Context, build api.TemplateBuildResponse, scope EnvironmentScopeOptions) (api.TemplateBuildResponse, error) {
	original := build
	for {
		switch build.Status {
		case "succeeded":
			if !build.Ready {
				return build, fmt.Errorf("template %s preparation is no longer eligible; request another refresh", build.TemplateID)
			}
			return build, nil
		case "failed", "cancelled":
			return build, fmt.Errorf("template %s preparation %s: %s", build.TemplateID, build.Status, build.ErrorCode)
		case "queued", "running":
		default:
			return build, fmt.Errorf("unknown preparation state %q", build.Status)
		}
		if build.ErrorCode == "deadline_exceeded" {
			return build, fmt.Errorf("template %s preparation deadline exceeded", build.TemplateID)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return build, ctx.Err()
		case <-timer.C:
		}
		next, err := c.GetTemplateBuild(ctx, original.TemplateID, original.ImageBuildID, original.DeploymentID, scope)
		if err != nil {
			return build, err
		}
		if next.TemplateID != original.TemplateID || next.DeploymentID != original.DeploymentID || next.ImageSpecID != original.ImageSpecID || next.ImageBuildID != original.ImageBuildID {
			return build, fmt.Errorf("preparation observation changed build identity")
		}
		build = next
	}
}
