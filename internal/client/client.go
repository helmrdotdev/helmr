package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/httpclient"
)

type Client struct {
	transport           *httpclient.Transport
	bearer              string
	sessionScopedRoutes bool
}

type options struct {
	bearer              string
	httpClient          *http.Client
	sessionScopedRoutes bool
}

type Option func(*options)

func WithBearerToken(token string) Option {
	return func(options *options) { options.bearer = token }
}

func WithSessionScopedRoutes() Option {
	return func(options *options) { options.sessionScopedRoutes = true }
}

func WithHTTPClient(httpClient *http.Client) Option {
	return func(options *options) { options.httpClient = httpClient }
}

func New(baseURL string, opts ...Option) (*Client, error) {
	var config options
	for _, option := range opts {
		option(&config)
	}
	transport, err := httpclient.New(baseURL, config.httpClient)
	if err != nil {
		return nil, err
	}
	return &Client{transport: transport, bearer: config.bearer, sessionScopedRoutes: config.sessionScopedRoutes}, nil
}

func (c *Client) UsesSessionScopedRoutes() bool { return c.sessionScopedRoutes }

func (c *Client) postJSON(ctx context.Context, path string, in any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(in); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) patchJSON(ctx context.Context, path string, in any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(in); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPatch, path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) deleteJSON(ctx context.Context, path string, in any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(in); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodDelete, path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) newRequest(ctx context.Context, method string, path string, body io.Reader) (*http.Request, error) {
	return c.transport.Request(ctx, method, path, body, c.bearer)
}

func (c *Client) doJSON(req *http.Request, out any) error { return c.transport.DoJSON(req, out) }
