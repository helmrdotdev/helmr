package workerclient

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) RegisterAgentSaveObject(ctx context.Context, request workerapi.AgentSaveObject) error {
	return c.postWorkerJSON(ctx, "/worker/v1/computer-saves/objects/register", request, nil)
}
func (c *Client) CertifyAgentSaveObject(ctx context.Context, request workerapi.AgentSaveObject) error {
	return c.postWorkerJSON(ctx, "/worker/v1/computer-saves/objects/certify", request, nil)
}
func (c *Client) CaptureAgentSave(ctx context.Context, request workerapi.AgentSavePublication) error {
	return c.postWorkerJSON(ctx, "/worker/v1/computer-saves/capture", request, nil)
}
func (c *Client) PublishAgentSave(ctx context.Context, request workerapi.AgentSavePublication) error {
	return c.postWorkerJSON(ctx, "/worker/v1/computer-saves/publish", request, nil)
}

func (c *Client) NextAgentSave(ctx context.Context, request workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	var result workerapi.AgentSavePending
	err := c.postWorkerJSON(ctx, "/worker/v1/computer-saves/next", request, &result)
	return result, err
}
