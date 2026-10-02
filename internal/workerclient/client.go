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

const hostCredentialRequestTimeout = 10 * time.Second

type Client struct {
	transport *httpclient.Transport
	auth      authentication
}

type authentication struct {
	workerHostID string
	secret       string
	serviceID    string
	credential   string
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
	_, err := c.hostCredential(ctx)
	return err
}

func (c *Client) postJSON(ctx context.Context, path string, bearer string, in any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(in); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.transport.Request(ctx, http.MethodPost, path, &body, bearer)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.transport.DoJSON(req, out)
}

func (c *Client) postWorkerJSON(ctx context.Context, path string, in any, out any) error {
	var payload []byte
	if in != nil {
		var err error
		payload, err = json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	for attempt := range 2 {
		credential, err := c.hostCredential(ctx)
		if err != nil {
			return err
		}
		req, err := c.transport.Request(ctx, http.MethodPost, path, bytes.NewReader(payload), credential)
		if err != nil {
			return err
		}
		if in != nil {
			req.Header.Set("content-type", "application/json")
		}
		err = c.transport.DoJSON(req, out)
		if attempt == 0 && httpclient.IsStatus(err, http.StatusUnauthorized) {
			c.invalidateHostCredential(credential)
			continue
		}
		return err
	}
	return errors.New("worker request retry exhausted")
}

func (c *Client) getWorkerJSON(ctx context.Context, path string, out any) error {
	for attempt := range 2 {
		credential, err := c.hostCredential(ctx)
		if err != nil {
			return err
		}
		req, err := c.transport.Request(ctx, http.MethodGet, path, nil, credential)
		if err != nil {
			return err
		}
		err = c.transport.DoJSON(req, out)
		if attempt == 0 && httpclient.IsStatus(err, http.StatusUnauthorized) {
			c.invalidateHostCredential(credential)
			continue
		}
		return err
	}
	return errors.New("worker request retry exhausted")
}

func (c *Client) invalidateHostCredential(credential string) {
	c.auth.mu.Lock()
	defer c.auth.mu.Unlock()
	if c.auth.credential == credential {
		c.auth.credential = ""
		c.auth.expiresAt = time.Time{}
	}
}

func (c *Client) hostCredential(ctx context.Context) (string, error) {
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
		if c.auth.credential != "" && time.Now().Add(30*time.Second).Before(c.auth.expiresAt) {
			credential := c.auth.credential
			c.auth.mu.Unlock()
			return credential, nil
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

		credential, expiresAt, err := c.requestHostCredential(ctx)
		c.auth.mu.Lock()
		if err == nil {
			c.auth.credential = credential
			c.auth.expiresAt = expiresAt
		}
		close(done)
		c.auth.refreshDone = nil
		c.auth.mu.Unlock()
		return credential, err
	}
}

// HostAuthorityRejectedError means the durable host secret or service identity
// cannot issue a credential. A rejected ordinary request may instead have raced
// a claim-version change and must not be classified as lost host authority.
type HostAuthorityRejectedError struct{ Err error }

func (e HostAuthorityRejectedError) Error() string                 { return e.Err.Error() }
func (e HostAuthorityRejectedError) Unwrap() error                 { return e.Err }
func (e HostAuthorityRejectedError) WorkerAuthorityRejected() bool { return true }

func (c *Client) requestHostCredential(ctx context.Context) (string, time.Time, error) {
	if c.auth.serviceID == "" {
		return "", time.Time{}, errors.New("worker service id is required")
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(workerapi.HostCredentialRequest{
		WorkerHostID: c.auth.workerHostID, WorkerHostSecret: c.auth.secret,
		ServiceID: c.auth.serviceID, APIVersion: workerapi.APIVersion,
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("encode worker host credential request: %w", err)
	}
	credentialCtx, cancel := context.WithTimeout(ctx, hostCredentialRequestTimeout)
	defer cancel()
	req, err := c.transport.Request(credentialCtx, http.MethodPost, "/worker/v1/instance/credential", &body, "")
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("content-type", "application/json")
	var response workerapi.HostCredentialResponse
	if err := c.transport.DoJSON(req, &response); err != nil {
		if httpclient.IsStatus(err, http.StatusUnauthorized) || httpclient.IsStatus(err, http.StatusForbidden) {
			err = HostAuthorityRejectedError{Err: err}
		}
		return "", time.Time{}, err
	}
	if response.Credential == "" {
		return "", time.Time{}, errors.New("worker host credential is empty")
	}
	if response.ExpiresInSeconds <= 0 {
		return "", time.Time{}, errors.New("worker host credential response expires_in_seconds must be positive")
	}
	return response.Credential, time.Now().Add(time.Duration(response.ExpiresInSeconds) * time.Second), nil
}
