package workerclient

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Release names the Control Plane's own release; it is nil for a Control
// Plane built from source.
func (c *Client) Release(ctx context.Context) (*workerapi.Release, error) {
	var result workerapi.ReleaseResponse
	err := c.getWorkerJSON(ctx, "/worker/v1/release", &result)
	return result.Release, err
}

// ReleaseRead grants a read of one object of the Control Plane's release.
func (c *Client) ReleaseRead(ctx context.Context, digest string) (workerapi.StorageReadGrant, error) {
	var result workerapi.StorageReadGrant
	err := c.postWorkerJSON(ctx, "/worker/v1/release/reads", workerapi.ReleaseReadRequest{Digest: digest}, &result)
	return result, err
}
