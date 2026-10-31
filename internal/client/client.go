package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/httpclient"
)

type Client struct {
	transport           *httpclient.Transport
	bearer              string
	sessionScopedRoutes bool
	expectedScope       EnvironmentScopeOptions
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

// ExpectAPIKeyScope names the Project and Environment later API-key requests
// expect to act in. The server rejects a request when the key is scoped
// elsewhere; an empty value expects nothing.
func (c *Client) ExpectAPIKeyScope(scope EnvironmentScopeOptions) { c.expectedScope = scope }

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
	req, err := c.transport.Request(ctx, method, path, body, c.bearer)
	if err != nil {
		return nil, err
	}
	if c.expectedScope.ProjectID != "" {
		req.Header.Set("X-Helmr-Project", c.expectedScope.ProjectID)
	}
	if c.expectedScope.EnvironmentID != "" {
		req.Header.Set("X-Helmr-Environment", c.expectedScope.EnvironmentID)
	}
	return req, nil
}

func (c *Client) doJSON(req *http.Request, out any) error { return c.transport.DoJSON(req, out) }

// invocationKey is resolved before request encoding, so replaying its body keeps
// one durable identity while a separate invocation receives a new identity.
func invocationKey(key string) string {
	if key == "" {
		return uuid.NewV7().String()
	}
	return key
}
