package workerclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const hostCredentialRequestTimeout = 10 * time.Second

type Client struct {
	network   *workerapi.NetworkCapabilities
	transport *httpclient.Transport
	auth      authentication
}

type authentication struct {
	workerHostID string
	key          ed25519.PrivateKey
	serviceID    string
	credential   string
	workerEpoch  int64
	expiresAt    time.Time
	refreshDone  chan struct{}
	mu           sync.Mutex
}

type options struct {
	network      *workerapi.NetworkCapabilities
	httpClient   *http.Client
	workerHostID string
	key          ed25519.PrivateKey
	serviceID    string
}

type Option func(*options)

func WithNetwork(installation networkpolicy.Installation) Option {
	return func(options *options) {
		options.network = &workerapi.NetworkCapabilities{Version: workerapi.NetworkPolicyVersion, Installation: installation.Canonical()}
	}
}

func WithHTTPClient(httpClient *http.Client) Option {
	return func(options *options) { options.httpClient = httpClient }
}

// WithAuth authenticates as the worker host with its machine key, which signs
// each host credential challenge and never leaves the client.
func WithAuth(workerHostID string, key ed25519.PrivateKey) Option {
	return func(options *options) {
		options.workerHostID = workerHostID
		options.key = key
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
	return &Client{transport: transport, network: config.network, auth: authentication{
		workerHostID: config.workerHostID,
		key:          config.key,
		serviceID:    config.serviceID,
	}}, nil
}

func (c *Client) AuthenticateWorker(ctx context.Context) error {
	_, err := c.hostCredential(ctx)
	return err
}

// HostIdentity returns the incarnation established by AuthenticateWorker. It is
// retained across credential refreshes so physical owners cannot change epochs.
func (c *Client) HostIdentity() (string, int64, error) {
	c.auth.mu.Lock()
	defer c.auth.mu.Unlock()
	if c.auth.workerEpoch <= 0 {
		return "", 0, errors.New("worker host has not authenticated")
	}
	return c.auth.workerHostID, c.auth.workerEpoch, nil
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
	return c.postWorkerJSONStatus(ctx, path, in, out, false)
}

func (c *Client) postWorkerJSONStatus(ctx context.Context, path string, in any, out any, retainStatus bool) error {
	var payload []byte
	if in != nil {
		var body bytes.Buffer
		encoder := json.NewEncoder(&body)
		// This is application/json, never embedded HTML. Preserve bounded
		// runtime JSON payloads without expanding '<', '>' and '&' sixfold.
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(in); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = body.Bytes()
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
		if retainStatus {
			err = c.transport.DoJSONWithStatus(req, out)
		} else {
			err = c.transport.DoJSON(req, out)
		}
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
		if len(c.auth.key) != ed25519.PrivateKeySize {
			c.auth.mu.Unlock()
			return "", errors.New("worker machine key is required")
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

		credential, expiresAt, epoch, err := c.requestHostCredential(ctx)
		c.auth.mu.Lock()
		if err == nil && c.auth.workerEpoch != 0 && c.auth.workerEpoch != epoch {
			err = HostAuthorityRejectedError{Err: errors.New("worker host epoch changed within the service process")}
		}
		if err == nil {
			c.auth.workerEpoch = epoch
			c.auth.credential = credential
			c.auth.expiresAt = expiresAt
		}
		close(done)
		c.auth.refreshDone = nil
		c.auth.mu.Unlock()
		return credential, err
	}
}

// HostAuthorityRejectedError means the machine key or service identity
// cannot issue a credential. A rejected ordinary request may instead have raced
// a claim-version change and must not be classified as lost host authority.
type HostAuthorityRejectedError struct{ Err error }

func (e HostAuthorityRejectedError) Error() string                 { return e.Err.Error() }
func (e HostAuthorityRejectedError) Unwrap() error                 { return e.Err }
func (e HostAuthorityRejectedError) WorkerAuthorityRejected() bool { return true }

func (c *Client) requestHostCredential(ctx context.Context) (string, time.Time, int64, error) {
	if c.auth.serviceID == "" {
		return "", time.Time{}, 0, errors.New("worker service id is required")
	}
	credentialCtx, cancel := context.WithTimeout(ctx, hostCredentialRequestTimeout)
	defer cancel()
	var challenge workerapi.HostChallengeResponse
	if err := c.postUnauthenticated(credentialCtx, "/worker/v1/instance/challenge", workerapi.HostChallengeRequest{
		APIVersion: workerapi.APIVersion, WorkerHostID: c.auth.workerHostID, ServiceID: c.auth.serviceID,
	}, &challenge); err != nil {
		return "", time.Time{}, 0, err
	}
	if challenge.Challenge == "" {
		return "", time.Time{}, 0, errors.New("worker host challenge is empty")
	}
	var response workerapi.HostCredentialResponse
	if err := c.postUnauthenticated(credentialCtx, "/worker/v1/instance/credential", workerapi.HostCredentialRequest{
		APIVersion: workerapi.APIVersion, WorkerHostID: c.auth.workerHostID, ServiceID: c.auth.serviceID,
		Challenge: challenge.Challenge, Signature: ed25519.Sign(c.auth.key, auth.HostChallengeMessage(challenge.Challenge)),
	}, &response); err != nil {
		if httpclient.IsStatus(err, http.StatusUnauthorized) || httpclient.IsStatus(err, http.StatusForbidden) {
			err = HostAuthorityRejectedError{Err: err}
		}
		return "", time.Time{}, 0, err
	}
	if response.Credential == "" {
		return "", time.Time{}, 0, errors.New("worker host credential is empty")
	}
	if response.ExpiresInSeconds <= 0 {
		return "", time.Time{}, 0, errors.New("worker host credential response expires_in_seconds must be positive")
	}
	if response.WorkerEpoch <= 0 {
		return "", time.Time{}, 0, errors.New("worker host credential response worker_epoch must be positive")
	}
	return response.Credential, time.Now().Add(time.Duration(response.ExpiresInSeconds) * time.Second), response.WorkerEpoch, nil
}

func (c *Client) postUnauthenticated(ctx context.Context, path string, request, response any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(request); err != nil {
		return fmt.Errorf("encode %s request: %w", path, err)
	}
	req, err := c.transport.Request(ctx, http.MethodPost, path, &body, "")
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.transport.DoJSON(req, response)
}
