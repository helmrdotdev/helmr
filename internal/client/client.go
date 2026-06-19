package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/version"
)

type HTTPError struct {
	StatusCode int
	Status     string
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return e.Status
	}
	return e.Status + ": " + e.Message
}

func IsStatus(err error, statusCode int) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == statusCode
}

type Client struct {
	baseURL             *url.URL
	bearer              string
	httpClient          *http.Client
	clientName          string
	clientVersion       string
	sessionScopedRoutes bool
	worker              workerAuth
}

type workerAuth struct {
	workerInstanceID string
	secret           string
	token            string
	expiresAt        time.Time
	refreshDone      chan struct{}
	mu               sync.Mutex
}

const workerTokenRequestTimeout = 10 * time.Second

type Option func(*Client)

func WithBearerToken(token string) Option {
	return func(client *Client) {
		client.bearer = token
	}
}

func WithSessionScopedRoutes() Option {
	return func(client *Client) {
		client.sessionScopedRoutes = true
	}
}

func (c *Client) UsesSessionScopedRoutes() bool {
	return c.sessionScopedRoutes
}

func WithWorkerAuth(workerInstanceID string, secret string) Option {
	return func(client *Client) {
		client.worker.workerInstanceID = workerInstanceID
		client.worker.secret = secret
	}
}

func WithHTTPClient(httpClient *http.Client) Option {
	return func(client *Client) {
		client.httpClient = httpClient
	}
}

func WithClientIdentity(name string, version string) Option {
	return func(client *Client) {
		client.clientName = strings.TrimSpace(name)
		client.clientVersion = strings.TrimSpace(version)
	}
}

func New(baseURL string, opts ...Option) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("base URL must include scheme and host")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported base URL scheme %q; expected http or https", parsed.Scheme)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("base URL must not include query or fragment")
	}
	if err := rejectPlaintextNonLoopbackURL(parsed); err != nil {
		return nil, err
	}
	client := &Client{
		baseURL:       parsed,
		httpClient:    http.DefaultClient,
		clientName:    "go",
		clientVersion: strings.TrimSpace(version.Version),
	}
	for _, opt := range opts {
		opt(client)
	}
	if client.httpClient == nil {
		client.httpClient = http.DefaultClient
	}
	client.httpClient = withSecureRedirects(client.httpClient)
	return client, nil
}

func withSecureRedirects(httpClient *http.Client) *http.Client {
	copied := *httpClient
	previous := copied.CheckRedirect
	copied.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := rejectPlaintextNonLoopbackURL(req.URL); err != nil {
			return err
		}
		if previous != nil {
			return previous(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &copied
}

func rejectPlaintextNonLoopbackURL(u *url.URL) error {
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("refusing to send credentials over plaintext non-loopback URL %s", u.Redacted())
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) postWorkerJSON(ctx context.Context, path string, in any, out any) error {
	token, err := c.workerToken(ctx)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(in); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.newRequestWithBearer(ctx, http.MethodPost, path, &body, token)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) getWorkerJSON(ctx context.Context, path string, out any) error {
	token, err := c.workerToken(ctx)
	if err != nil {
		return err
	}
	req, err := c.newRequestWithBearer(ctx, http.MethodGet, path, nil, token)
	if err != nil {
		return err
	}
	return c.doJSON(req, out)
}

func (c *Client) workerToken(ctx context.Context) (string, error) {
	for {
		c.worker.mu.Lock()
		if strings.TrimSpace(c.worker.workerInstanceID) == "" {
			c.worker.mu.Unlock()
			return "", fmt.Errorf("worker instance id is required")
		}
		if strings.TrimSpace(c.worker.secret) == "" {
			c.worker.mu.Unlock()
			return "", fmt.Errorf("worker secret is required")
		}
		if c.worker.token != "" && time.Now().Add(30*time.Second).Before(c.worker.expiresAt) {
			token := c.worker.token
			c.worker.mu.Unlock()
			return token, nil
		}
		if done := c.worker.refreshDone; done != nil {
			c.worker.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		c.worker.refreshDone = done
		c.worker.mu.Unlock()

		token, expiresAt, err := c.requestWorkerToken(ctx)
		c.worker.mu.Lock()
		if err == nil {
			c.worker.token = token
			c.worker.expiresAt = expiresAt
		}
		close(done)
		c.worker.refreshDone = nil
		c.worker.mu.Unlock()
		return token, err
	}
}

func (c *Client) requestWorkerToken(ctx context.Context) (string, time.Time, error) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(api.WorkerTokenRequest{WorkerInstanceID: c.worker.workerInstanceID, WorkerInstanceSecret: c.worker.secret}); err != nil {
		return "", time.Time{}, fmt.Errorf("encode worker token request: %w", err)
	}
	tokenCtx, cancel := context.WithTimeout(ctx, workerTokenRequestTimeout)
	defer cancel()
	req, err := c.newRequest(tokenCtx, http.MethodPost, "/api/worker/auth/token", &body)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("content-type", "application/json")
	var response api.WorkerTokenResponse
	if err := c.doJSON(req, &response); err != nil {
		return "", time.Time{}, err
	}
	if response.Token == "" {
		return "", time.Time{}, fmt.Errorf("worker auth token is empty")
	}
	if response.ExpiresInSeconds <= 0 {
		return "", time.Time{}, fmt.Errorf("worker auth response expires_in_seconds must be positive")
	}
	return response.Token, time.Now().Add(time.Duration(response.ExpiresInSeconds) * time.Second), nil
}

func (c *Client) postJSON(ctx context.Context, path string, in any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(in); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.newRequestWithBearer(ctx, http.MethodPost, path, &body, c.bearer)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) putJSON(ctx context.Context, path string, in any, out any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(in); err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := c.newRequestWithBearer(ctx, http.MethodPut, path, &body, c.bearer)
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
	req, err := c.newRequestWithBearer(ctx, http.MethodPatch, path, &body, c.bearer)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) newRequest(ctx context.Context, method string, path string, body io.Reader) (*http.Request, error) {
	return c.newRequestWithBearer(ctx, method, path, body, c.bearer)
}

func (c *Client) newRequestWithBearer(ctx context.Context, method string, path string, body io.Reader, bearer string) (*http.Request, error) {
	endpoint := *c.baseURL
	pathOnly, rawQuery, _ := strings.Cut(path, "?")
	escapedPath := joinPath(c.baseURL.EscapedPath(), pathOnly)
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		return nil, err
	}
	endpoint.Path = decodedPath
	if decodedPath != escapedPath {
		endpoint.RawPath = escapedPath
	} else {
		endpoint.RawPath = ""
	}
	endpoint.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set(api.APIVersionHeader, api.CurrentAPIVersion)
	if c.clientVersion != "" {
		req.Header.Set(api.ClientVersionHeader, c.clientVersion)
		switch c.clientName {
		case "cli":
			req.Header.Set(api.CLIVersionHeader, c.clientVersion)
		case "sdk":
			req.Header.Set(api.SDKVersionHeader, c.clientVersion)
		}
	}
	if bearer != "" {
		req.Header.Set("authorization", "Bearer "+bearer)
	}
	return req, nil
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read error response: %w", err)
	}
	return decodeErrorBody(resp.StatusCode, resp.Status, body)
}

func decodeErrorBody(statusCode int, status string, body []byte) error {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != "" {
		return &HTTPError{StatusCode: statusCode, Status: status, Message: payload.Error}
	}
	return &HTTPError{StatusCode: statusCode, Status: status}
}

func joinPath(basePath string, path string) string {
	basePath = strings.TrimRight(basePath, "/")
	path = "/" + strings.TrimLeft(path, "/")
	if basePath == "" {
		return path
	}
	return basePath + path
}
