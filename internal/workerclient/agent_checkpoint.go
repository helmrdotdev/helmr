package workerclient

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) RegisterAgentCheckpoint(ctx context.Context, request workerapi.AgentCheckpointPublication) error {
	return c.postWorkerJSON(ctx, "/worker/v1/agent-computers/checkpoint/register", request, nil)
}
func (c *Client) CompleteAgentCheckpoint(ctx context.Context, request workerapi.AgentCheckpointPublication) error {
	return c.postWorkerJSON(ctx, "/worker/v1/agent-computers/checkpoint/complete", request, nil)
}
func (c *Client) ReadAgentCheckpoint(ctx context.Context, request workerapi.AllocationIdentity) (computercheckpoint.Manifest, error) {
	var result computercheckpoint.Manifest
	err := c.postWorkerJSON(ctx, "/worker/v1/agent-computers/checkpoint/read", request, &result)
	return result, err
}
