package workerclient

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) DeliverBootSeedAllocation(ctx context.Context, request workerapi.AllocationIdentity) (workerapi.BootSeedAllocationDelivery, error) {
	var response workerapi.BootSeedAllocationDelivery
	err := c.postWorkerJSON(ctx, "/worker/v1/allocations/boot-seed/deliver", workerapi.AllocationRequest{AllocationIdentity: request, Network: c.network}, &response)
	return response, err
}

func (c *Client) BootSeedStart(ctx context.Context, request workerapi.BootSeedExecutor) (workerapi.ComputerAllocationSource, error) {
	request.Network = c.network

	var response workerapi.ComputerAllocationSource
	err := c.postWorkerJSON(ctx, "/worker/v1/allocations/boot-seed/start", request, &response)
	if err != nil {
		response.Disk.Clear()
		return workerapi.ComputerAllocationSource{}, err
	}
	return response, nil
}

func (c *Client) RenewBootSeed(ctx context.Context, request workerapi.BootSeedExecutor) (workerapi.PreparationRenewal, error) {
	request.Network = c.network

	var response workerapi.PreparationRenewal
	err := c.postWorkerJSON(ctx, "/worker/v1/allocations/boot-seed/renew", request, &response)
	return response, err
}

func (c *Client) PublishBootSeed(ctx context.Context, request workerapi.BootSeedPublication) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/boot-seed/publish", request, nil)
}

func (c *Client) FailBootSeed(ctx context.Context, request workerapi.BootSeedFailure) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/boot-seed/fail", request, nil)
}

func (c *Client) ReclaimBootSeed(ctx context.Context, request workerapi.BootSeedExecutor) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/boot-seed/reclaim", request, nil)
}

func (c *Client) ObserveBootSeedStopped(ctx context.Context, request workerapi.AllocationIdentity) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/boot-seed/stopped", request, nil)
}
