package client

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/api"
)

func (c *Client) StartDeviceCode(ctx context.Context) (api.DeviceStartResponse, error) {
	var response api.DeviceStartResponse
	if err := c.postJSON(ctx, "/api/auth/device/start", struct{}{}, &response); err != nil {
		return api.DeviceStartResponse{}, err
	}
	return response, nil
}

func (c *Client) ExchangeDeviceCode(ctx context.Context, deviceCode string) (api.DeviceTokenResponse, error) {
	var response api.DeviceTokenResponse
	if err := c.postJSON(ctx, "/api/auth/device/token", api.DeviceTokenRequest{DeviceCode: deviceCode}, &response); err != nil {
		return api.DeviceTokenResponse{}, err
	}
	return response, nil
}

func (c *Client) Logout(ctx context.Context) error {
	return c.postJSON(ctx, "/api/auth/logout", struct{}{}, nil)
}
