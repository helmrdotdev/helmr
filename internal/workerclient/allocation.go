package workerclient

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (client *Client) ListAllocations(ctx context.Context, after *workerapi.AllocationIdentity) (workerapi.AllocationListResponse, error) {
	var response workerapi.AllocationListResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/allocations/list", workerapi.AllocationListRequest{After: after}, &response)
	return response, err
}
func (client *Client) DeliverPreparationAllocation(ctx context.Context, identity workerapi.AllocationIdentity) (workerapi.PreparationAllocationDelivery, error) {
	var response workerapi.PreparationAllocationDelivery
	err := client.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/deliver", identity, &response)
	return response, err
}
func (client *Client) DeliverComputerAllocation(ctx context.Context, identity workerapi.AllocationIdentity) (workerapi.ComputerAllocationDelivery, error) {
	var response workerapi.ComputerAllocationDelivery
	err := client.postWorkerJSON(ctx, "/worker/v1/allocations/computer/deliver", identity, &response)
	return response, err
}
func (client *Client) ObserveFreshComputerReady(ctx context.Context, receipt workerapi.ComputerAllocationReady) error {
	return client.postWorkerJSON(ctx, "/worker/v1/allocations/computer/ready", receipt, nil)
}
func (client *Client) ObservePreparationStopped(ctx context.Context, identity workerapi.AllocationIdentity) error {
	return client.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/stopped", identity, nil)
}

func (client *Client) ListComputerProcesses(ctx context.Context, identity workerapi.AllocationIdentity, after *workerapi.ProcessIdentity) (workerapi.ComputerProcessesResponse, error) {
	var response workerapi.ComputerProcessesResponse
	err := client.postWorkerJSON(ctx, "/worker/v1/allocations/computer/processes", workerapi.ComputerProcessesRequest{Identity: identity, After: after}, &response)
	return response, err
}

func (client *Client) ComputerAllocationSource(ctx context.Context, identity workerapi.AllocationIdentity) (workerapi.ComputerAllocationSource, error) {
	var response workerapi.ComputerAllocationSource
	err := client.postWorkerJSON(ctx, "/worker/v1/allocations/computer/source", identity, &response)
	if err != nil {
		response.Disk.Clear()
		return workerapi.ComputerAllocationSource{}, err
	}
	return response, nil
}
