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
	baseURL    *url.URL
	bearer     string
	httpClient *http.Client
	worker     workerAuth
}

type workerAuth struct {
	workerHostID string
	secret       string
	token        string
	expiresAt    time.Time
	mu           sync.Mutex
}

type Option func(*Client)

func WithBearerToken(token string) Option {
	return func(client *Client) {
		client.bearer = token
	}
}

func WithWorkerAuth(workerHostID string, secret string) Option {
	return func(client *Client) {
		client.worker.workerHostID = workerHostID
		client.worker.secret = secret
	}
}

func WithHTTPClient(httpClient *http.Client) Option {
	return func(client *Client) {
		client.httpClient = httpClient
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
		baseURL:    parsed,
		httpClient: http.DefaultClient,
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
	c.worker.mu.Lock()
	defer c.worker.mu.Unlock()
	if strings.TrimSpace(c.worker.workerHostID) == "" {
		return "", fmt.Errorf("worker instance id is required")
	}
	if strings.TrimSpace(c.worker.secret) == "" {
		return "", fmt.Errorf("worker secret is required")
	}
	if c.worker.token != "" && time.Now().Add(30*time.Second).Before(c.worker.expiresAt) {
		return c.worker.token, nil
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(api.WorkerTokenRequest{WorkerInstanceID: c.worker.workerHostID, WorkerInstanceSecret: c.worker.secret}); err != nil {
		return "", fmt.Errorf("encode worker token request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/worker/auth/token", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	var response api.WorkerTokenResponse
	if err := c.doJSON(req, &response); err != nil {
		return "", err
	}
	if response.Token == "" {
		return "", fmt.Errorf("worker auth response token is empty")
	}
	if response.ExpiresInSeconds <= 0 {
		return "", fmt.Errorf("worker auth response expires_in_seconds must be positive")
	}
	c.worker.token = response.Token
	c.worker.expiresAt = time.Now().Add(time.Duration(response.ExpiresInSeconds) * time.Second)
	return c.worker.token, nil
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
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil && payload.Error != "" {
		return &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Message: payload.Error}
	}
	return &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status}
}

func joinPath(basePath string, path string) string {
	basePath = strings.TrimRight(basePath, "/")
	path = "/" + strings.TrimLeft(path, "/")
	if basePath == "" {
		return path
	}
	return basePath + path
}
