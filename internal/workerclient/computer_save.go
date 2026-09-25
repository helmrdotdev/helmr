package workerclient

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) BeginComputerSave(ctx context.Context, request workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error) {
	var response workerapi.ComputerSaveBeginResponse
	err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-saves/begin", request, &response)
	return response, err
}
func (c *Client) AbandonComputerSave(ctx context.Context, request workerapi.ComputerSaveBeginRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-saves/abandon", request, &struct{}{})
}
func (c *Client) PublishComputerSave(ctx context.Context, request workerapi.ComputerSavePublicationRequest) (workerapi.ComputerSavePublicationResponse, error) {
	var response workerapi.ComputerSavePublicationResponse
	err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-saves/publish", request, &response)
	return response, err
}
func (c *Client) AdoptComputerSave(ctx context.Context, request workerapi.ComputerSavePublicationRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-saves/adopt", request, &struct{}{})
}
func (c *Client) RegisterComputerSaveObject(ctx context.Context, request workerapi.ComputerSaveObjectRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-saves/objects/register", request, &struct{}{})
}
func (c *Client) CertifyComputerSaveObject(ctx context.Context, request workerapi.ComputerSaveObjectRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-saves/objects/certify", request, &struct{}{})
}
func (c *Client) ReuseComputerSaveObject(ctx context.Context, request workerapi.ComputerSaveObjectRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/run/computer-saves/objects/reuse", request, &struct{}{})
}
