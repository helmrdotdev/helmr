package workerclient

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (c *Client) PreparationSource(ctx context.Context, request workerapi.PreparationSourceRequest) (workerapi.PreparationSource, error) {
	var result workerapi.PreparationSource
	err := c.postWorkerJSON(ctx, "/worker/v1/preparation-sources", request, &result)
	if err != nil {
		result.Source.Disk.Clear()
		return workerapi.PreparationSource{}, err
	}
	return result, nil
}
