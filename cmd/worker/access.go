package main

import (
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func newControlPlaneClient(baseURL string, opts ...workerclient.Option) (*workerclient.Client, error) {
	httpClient, err := config.AccessHTTPClient(nil)
	if err != nil {
		return nil, err
	}
	opts = append(opts, workerclient.WithHTTPClient(httpClient))
	return workerclient.New(baseURL, opts...)
}
