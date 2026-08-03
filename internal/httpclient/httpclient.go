package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
)

type Error struct {
	StatusCode int
	Status     string
	Message    string
	Code       string
	Retryable  bool
	RequestID  string
}

func (e *Error) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.StatusCode
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Status
	}
	return e.Status + ": " + e.Message
}

func IsStatus(err error, statusCode int) bool {
	var httpErr *Error
	return errors.As(err, &httpErr) && httpErr.StatusCode == statusCode
}

type Transport struct {
	baseURL    *url.URL
	httpClient *http.Client
}

func (t *Transport) BaseURL() string { return t.baseURL.String() }

func New(baseURL string, httpClient *http.Client) (*Transport, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("base URL must include scheme and host")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported base URL scheme %q; expected http or https", parsed.Scheme)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("base URL must not include query or fragment")
	}
	if err := rejectPlaintextNonLoopbackURL(parsed); err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Transport{baseURL: parsed, httpClient: withSecureRedirects(httpClient)}, nil
}

func (t *Transport) Request(ctx context.Context, method string, path string, body io.Reader, bearer string) (*http.Request, error) {
	endpoint := *t.baseURL
	pathOnly, rawQuery, _ := strings.Cut(path, "?")
	escapedPath := joinPath(t.baseURL.EscapedPath(), pathOnly)
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
	if bearer != "" {
		req.Header.Set("authorization", "Bearer "+bearer)
	}
	return req, nil
}

// Do sends req and returns only successful HTTP responses. The caller owns the
// returned response body. Error responses are decoded and closed here.
func (t *Transport) Do(req *http.Request) (*http.Response, error) {
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

func (t *Transport) DoJSON(req *http.Request, out any) error {
	resp, err := t.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
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

func decodeError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read error response: %w", err)
	}
	var payload struct {
		Error     string `json:"error"`
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != "" {
		return &Error{StatusCode: resp.StatusCode, Status: resp.Status, Message: payload.Error, Code: payload.Code, Retryable: payload.Retryable, RequestID: payload.RequestID}
	}
	return &Error{StatusCode: resp.StatusCode, Status: resp.Status}
}

func joinPath(basePath string, path string) string {
	basePath = strings.TrimRight(basePath, "/")
	path = "/" + strings.TrimLeft(path, "/")
	if basePath == "" {
		return path
	}
	return basePath + path
}
