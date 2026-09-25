package workerclient

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) PublishInitialComputerGeneration(ctx context.Context, request workerapi.InitialComputerGenerationRequest) (workerapi.InitialComputerGenerationResponse, error) {
	var response workerapi.InitialComputerGenerationResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/runtime-instances/initialization/generation", request, &response); err != nil {
		return workerapi.InitialComputerGenerationResponse{}, err
	}
	if ids.Validate(response.ComputerID) != nil || ids.Validate(response.VersionID) != nil {
		return workerapi.InitialComputerGenerationResponse{}, errors.New("invalid computer generation publication response")
	}
	return response, nil
}
