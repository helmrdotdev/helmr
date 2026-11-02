package workerclient

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) StorageRead(ctx context.Context, request workerapi.StorageReadRequest) (workerapi.StorageReadGrant, error) {
	var result workerapi.StorageReadGrant
	err := c.postWorkerJSON(ctx, "/worker/v1/storage/reads", request, &result)
	return result, err
}

func (c *Client) StorageWrite(ctx context.Context, request workerapi.StorageWriteRequest) (workerapi.StorageWriteResponse, error) {
	var result workerapi.StorageWriteResponse
	err := c.postWorkerJSON(ctx, "/worker/v1/storage/writes", request, &result)
	return result, err
}

func (c *Client) StorageParts(ctx context.Context, request workerapi.StoragePartsRequest) (workerapi.StoragePartsResponse, error) {
	var result workerapi.StoragePartsResponse
	err := c.postWorkerJSON(ctx, "/worker/v1/storage/writes/parts", request, &result)
	return result, err
}

func (c *Client) StorageComplete(ctx context.Context, request workerapi.StorageCompleteRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/storage/writes/complete", request, nil)
}

func (c *Client) StorageAbort(ctx context.Context, request workerapi.StorageAbortRequest) error {
	return c.postWorkerJSON(ctx, "/worker/v1/storage/writes/abort", request, nil)
}

func (c *Client) CheckpointKey(ctx context.Context, request workerapi.CheckpointKeyRequest) ([]byte, error) {
	var result workerapi.ObjectKey
	err := c.postWorkerJSON(ctx, "/worker/v1/agent-computers/checkpoint/key", request, &result)
	return result.Key, err
}

func (c *Client) BootSeedKey(ctx context.Context, request workerapi.BootSeedKeyRequest) ([]byte, error) {
	var result workerapi.ObjectKey
	err := c.postWorkerJSON(ctx, "/worker/v1/boot-seeds/key", request, &result)
	return result.Key, err
}
