package workerclient

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) PublishInitialComputerVersion(ctx context.Context, request workerapi.InitialComputerVersionRequest) (workerapi.InitialComputerVersionResponse, error) {
	var response workerapi.InitialComputerVersionResponse
	if err := c.postWorkerJSON(ctx, "/worker/v1/run/computer-instances/initialization/version", request, &response); err != nil {
		return workerapi.InitialComputerVersionResponse{}, err
	}
	if ids.Validate(response.ComputerID) != nil || ids.Validate(response.VersionID) != nil {
		return workerapi.InitialComputerVersionResponse{}, errors.New("invalid computer version publication response")
	}
	return response, nil
}
