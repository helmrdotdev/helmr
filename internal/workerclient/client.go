package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const tokenRequestTimeout = 10 * time.Second

type Client struct {
	transport *httpclient.Transport
	auth      authentication
}

type authentication struct {
	workerInstanceID string
	secret           string
	serviceID        string
	protocolVersion  string
	supportsRun      bool
	supportsBuild    bool
	token            string
	expiresAt        time.Time
	refreshDone      chan struct{}
	mu               sync.Mutex
}

type options struct {
	httpClient       *http.Client
	workerInstanceID string
	secret           string
	serviceID        string
	protocolVersion  string
	supportsRun      bool
	supportsBuild    bool
}

type Option func(*options)

func WithHTTPClient(httpClient *http.Client) Option {
	return func(options *options) { options.httpClient = httpClient }
}

func WithAuth(workerInstanceID string, secret string) Option {
	return func(options *options) {
		options.workerInstanceID = workerInstanceID
		options.secret = secret
	}
}

func WithService(serviceID string, protocolVersion string, supportsRun bool, supportsBuild bool) Option {
	return func(options *options) {
		options.serviceID = strings.TrimSpace(serviceID)
		options.protocolVersion = strings.TrimSpace(protocolVersion)
		options.supportsRun = supportsRun
		options.supportsBuild = supportsBuild
	}
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
	return &Client{transport: transport, auth: authentication{
		workerInstanceID: config.workerInstanceID,
		secret:           config.secret,
		serviceID:        config.serviceID,
		protocolVersion:  config.protocolVersion,
		supportsRun:      config.supportsRun,
		supportsBuild:    config.supportsBuild,
	}}, nil
}

func (c *Client) AuthenticateWorker(ctx context.Context) error {
	_, err := c.token(ctx)
	return err
}

func (c *Client) postJSON(ctx context.Context, path string, in any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(in); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.transport.Request(ctx, http.MethodPost, path, &body, "")
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.transport.DoJSON(req, out)
}

func (c *Client) postWorkerJSON(ctx context.Context, path string, in any, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	for attempt := range 2 {
		token, err := c.token(ctx)
		if err != nil {
			return err
		}
		req, err := c.transport.Request(ctx, http.MethodPost, path, bytes.NewReader(payload), token)
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		err = c.transport.DoJSON(req, out)
		if attempt == 0 && httpclient.IsStatus(err, http.StatusUnauthorized) {
			c.invalidateToken(token)
			continue
		}
		return err
	}
	return errors.New("worker request retry exhausted")
}

func (c *Client) getWorkerJSON(ctx context.Context, path string, out any) error {
	for attempt := range 2 {
		token, err := c.token(ctx)
		if err != nil {
			return err
		}
		req, err := c.transport.Request(ctx, http.MethodGet, path, nil, token)
		if err != nil {
			return err
		}
		err = c.transport.DoJSON(req, out)
		if attempt == 0 && httpclient.IsStatus(err, http.StatusUnauthorized) {
			c.invalidateToken(token)
			continue
		}
		return err
	}
	return errors.New("worker request retry exhausted")
}

func (c *Client) invalidateToken(token string) {
	c.auth.mu.Lock()
	defer c.auth.mu.Unlock()
	if c.auth.token == token {
		c.auth.token = ""
		c.auth.expiresAt = time.Time{}
	}
}

func (c *Client) token(ctx context.Context) (string, error) {
	for {
		c.auth.mu.Lock()
		if strings.TrimSpace(c.auth.workerInstanceID) == "" {
			c.auth.mu.Unlock()
			return "", errors.New("worker instance id is required")
		}
		if strings.TrimSpace(c.auth.secret) == "" {
			c.auth.mu.Unlock()
			return "", errors.New("worker secret is required")
		}
		if c.auth.token != "" && time.Now().Add(30*time.Second).Before(c.auth.expiresAt) {
			token := c.auth.token
			c.auth.mu.Unlock()
			return token, nil
		}
		if done := c.auth.refreshDone; done != nil {
			c.auth.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		c.auth.refreshDone = done
		c.auth.mu.Unlock()

		token, expiresAt, err := c.requestToken(ctx)
		c.auth.mu.Lock()
		if err == nil {
			c.auth.token = token
			c.auth.expiresAt = expiresAt
		}
		close(done)
		c.auth.refreshDone = nil
		c.auth.mu.Unlock()
		return token, err
	}
}

func (c *Client) requestToken(ctx context.Context) (string, time.Time, error) {
	if c.auth.serviceID == "" || c.auth.protocolVersion == "" || !c.auth.supportsRun && !c.auth.supportsBuild {
		return "", time.Time{}, errors.New("worker service id, protocol version, and at least one role are required")
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(workerapi.TokenRequest{
		WorkerInstanceID: c.auth.workerInstanceID, WorkerInstanceSecret: c.auth.secret,
		ServiceID: c.auth.serviceID, ProtocolVersion: c.auth.protocolVersion,
		SupportsRun: c.auth.supportsRun, SupportsBuild: c.auth.supportsBuild,
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("encode worker token request: %w", err)
	}
	tokenCtx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()
	req, err := c.transport.Request(tokenCtx, http.MethodPost, "/api/worker/auth/token", &body, "")
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("content-type", "application/json")
	var response workerapi.TokenResponse
	if err := c.transport.DoJSON(req, &response); err != nil {
		return "", time.Time{}, err
	}
	if response.Token == "" {
		return "", time.Time{}, errors.New("worker auth token is empty")
	}
	if response.ExpiresInSeconds <= 0 {
		return "", time.Time{}, errors.New("worker auth response expires_in_seconds must be positive")
	}
	return response.Token, time.Now().Add(time.Duration(response.ExpiresInSeconds) * time.Second), nil
}
