package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	workerHostID string
	secret       string
	serviceID    string
	token        string
	expiresAt    time.Time
	refreshDone  chan struct{}
	mu           sync.Mutex
}

type options struct {
	httpClient   *http.Client
	workerHostID string
	secret       string
	serviceID    string
}

type Option func(*options)

func WithHTTPClient(httpClient *http.Client) Option {
	return func(options *options) { options.httpClient = httpClient }
}

func WithAuth(workerHostID string, secret string) Option {
	return func(options *options) {
		options.workerHostID = workerHostID
		options.secret = secret
	}
}

func WithService(serviceID string) Option {
	return func(options *options) {
		options.serviceID = strings.TrimSpace(serviceID)
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
		workerHostID: config.workerHostID,
		secret:       config.secret,
		serviceID:    config.serviceID,
	}}, nil
}

func (c *Client) AuthenticateWorker(ctx context.Context) error {
	_, err := c.token(ctx)
	return err
}

func (c *Client) postJSON(ctx context.Context, path string, bearer string, in any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(in); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.request(ctx, http.MethodPost, path, &body, bearer)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doJSON(req, out)
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
		req, err := c.request(ctx, http.MethodPost, path, bytes.NewReader(payload), token)
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		err = c.doJSON(req, out)
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
		req, err := c.request(ctx, http.MethodGet, path, nil, token)
		if err != nil {
			return err
		}
		err = c.doJSON(req, out)
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
		if strings.TrimSpace(c.auth.workerHostID) == "" {
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
	if c.auth.serviceID == "" {
		return "", time.Time{}, errors.New("worker service id is required")
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(workerapi.TokenRequest{
		WorkerHostID: c.auth.workerHostID, WorkerHostSecret: c.auth.secret,
		ServiceID: c.auth.serviceID,
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("encode worker token request: %w", err)
	}
	tokenCtx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()
	req, err := c.request(tokenCtx, http.MethodPost, "/worker/v1/instance/token", &body, "")
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("content-type", "application/json")
	var response workerapi.TokenResponse
	if err := c.doJSON(req, &response); err != nil {
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

// request builds a /worker/v1 request that names the contract this build
// speaks.
func (c *Client) request(ctx context.Context, method string, path string, body io.Reader, bearer string) (*http.Request, error) {
	req, err := c.transport.Request(ctx, method, path, body, bearer)
	if err != nil {
		return nil, err
	}
	req.Header.Set(workerapi.ContractHeader, workerapi.Contract)
	return req, nil
}

func (c *Client) doJSON(req *http.Request, out any) error {
	return contractMismatch(c.transport.DoJSON(req, out))
}

// contractMismatch turns the control plane's worker_contract_mismatch
// rejection into workerapi.ContractMismatchError. It is not a 401, so it
// neither refreshes nor discards credentials, and retrying cannot succeed
// until the worker or control plane is replaced.
func contractMismatch(err error) error {
	var httpErr *httpclient.Error
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusConflict || httpErr.Code != workerapi.ContractMismatchCode {
		return err
	}
	var details map[string]string
	if len(httpErr.Details) > 0 {
		if decodeErr := json.Unmarshal(httpErr.Details, &details); decodeErr != nil {
			return fmt.Errorf("%w (decode contract details: %v)", err, decodeErr)
		}
	}
	return workerapi.ContractMismatchError{
		Worker:       workerapi.Contract,
		ControlPlane: details[workerapi.ContractMismatchControlPlaneDetail],
	}
}

func asContractMismatch(err error) (workerapi.ContractMismatchError, bool) {
	var mismatch workerapi.ContractMismatchError
	return mismatch, errors.As(err, &mismatch)
}

// sensitiveContractMismatch recovers the contract mismatch from the error body
// of a sensitive request. Only the error code and the control plane's contract
// name are read; every other response stays status-only.
func sensitiveContractMismatch(status int, body []byte) error {
	if status != http.StatusConflict {
		return nil
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				ControlPlane string `json:"control_plane_contract"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Error.Code != workerapi.ContractMismatchCode {
		return nil
	}
	controlPlane := payload.Error.Details.ControlPlane
	if !contractName(controlPlane) {
		return nil
	}
	return workerapi.ContractMismatchError{Worker: workerapi.Contract, ControlPlane: controlPlane}
}

// contractName accepts a short printable ASCII contract identifier.
func contractName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	return true
}
