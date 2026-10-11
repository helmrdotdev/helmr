package workerclient

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/wire"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) RenewPreparation(ctx context.Context, request workerapi.PreparationExecutor) (workerapi.PreparationRenewal, error) {
	var response workerapi.PreparationRenewal
	err := c.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/renew", request, &response)
	return response, err
}
func (c *Client) PreparationWriteKey(ctx context.Context, request workerapi.PreparationExecutor) (workerapi.ComputerKeyMaterial, error) {
	var response workerapi.ComputerKeyMaterial
	body, err := c.preparationRequest(ctx, "/worker/v1/allocations/preparation/key", request, true, 4096)
	if err != nil {
		return response, err
	}
	defer clear(body)
	if json.Unmarshal(body, &response) != nil || ids.Validate(response.ID) != nil || len(response.Key) != 32 || response.Scope == "" || len(response.Scope) > 256 {
		clear(response.Key)
		return workerapi.ComputerKeyMaterial{}, errors.New("invalid preparation key response")
	}
	return response, nil
}
func (c *Client) FailPreparation(ctx context.Context, request workerapi.PreparationFailure) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/fail", request, nil)
}
func (c *Client) BeginPreparationCapture(ctx context.Context, request workerapi.PreparationCaptureBegin) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/capture/begin", request, nil)
}
func (c *Client) RegisterPreparationObject(ctx context.Context, request workerapi.PreparationObject) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/objects/register", request, nil)
}
func (c *Client) CertifyPreparationObject(ctx context.Context, request workerapi.PreparationObject) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/objects/certify", request, nil)
}
func (c *Client) RecordPreparationCapture(ctx context.Context, request workerapi.PreparationPublication) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/capture", request, nil)
}
func (c *Client) PublishPreparation(ctx context.Context, request workerapi.PreparationPublication) error {
	return c.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/publish", request, nil)
}

func (c *Client) PreparationSecrets(ctx context.Context, request workerapi.PreparationExecutor) (workerapi.PreparationSecrets, error) {
	var response workerapi.PreparationSecrets
	body, err := c.preparationRequest(ctx, "/worker/v1/allocations/preparation/secrets", request, true, 2*wire.PreparationControlFrameBytes)
	if err != nil {
		return response, err
	}
	defer clear(body)
	if json.Unmarshal(body, &response) != nil {
		for _, s := range response.Secrets {
			clear(s.Value)
		}
		return workerapi.PreparationSecrets{}, errors.New("invalid preparation Secrets response")
	}
	return response, nil
}

func (c *Client) PreparationStart(ctx context.Context, request workerapi.PreparationExecutor) (workerapi.PreparationStart, error) {
	var response workerapi.PreparationStart
	err := c.postWorkerJSON(ctx, "/worker/v1/allocations/preparation/start", request, &response)
	return response, err
}
