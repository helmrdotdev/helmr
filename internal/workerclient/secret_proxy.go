package workerclient

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"time"
)

func (c *Client) PrepareSecretProxy(ctx context.Context, request workerapi.SecretProxyRequest) (workerapi.SecretProxyPreparation, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var response workerapi.SecretProxyPreparation
	err := c.postWorkerJSON(ctx, "/worker/v1/run/secret-proxy/prepare", request, &response)
	return response, secretProxyError(err)
}

func (c *Client) ResolveSecretProxy(ctx context.Context, request workerapi.SecretProxyRequest) (workerapi.SecretProxyResolution, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var response workerapi.SecretProxyResolution
	err := c.postWorkerJSON(ctx, "/worker/v1/run/secret-proxy/resolve", request, &response)
	return response, secretProxyError(err)
}

func secretProxyError(err error) error {
	var response *httpclient.Error
	if errors.As(err, &response) && response.Message == secretproxy.ErrTrustExpired.Error() {
		return secretproxy.ErrTrustExpired
	}
	return err
}
