package deployment

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type upstreamRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip upstreamRoundTripper) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	return roundTrip(request)
}

func TestFetchUpstreamClassifiesPermanentAndTransientStatus(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		headers       http.Header
		diagnostic    string
		rawURL        string
		deterministic bool
		reason        workerapi.PlatformAcquisitionFailureReason
	}{
		{
			name:          "missing selector",
			status:        http.StatusNotFound,
			deterministic: true,
			reason:        workerapi.PlatformAcquisitionUnsupportedSelector,
		},
		{
			name:          "origin rejects request",
			status:        http.StatusForbidden,
			deterministic: true,
			reason:        workerapi.PlatformAcquisitionOriginRejected,
		},
		{
			name:   "origin rejects request with ordinary rate metadata",
			status: http.StatusForbidden,
			headers: http.Header{
				"X-Ratelimit-Remaining": []string{"12"},
				"X-Ratelimit-Reset":     []string{"1785235800"},
			},
			deterministic: true,
			reason:        workerapi.PlatformAcquisitionOriginRejected,
		},
		{
			name:    "origin rate limits request",
			status:  http.StatusForbidden,
			headers: http.Header{"X-Ratelimit-Remaining": []string{"0"}},
		},
		{
			name:       "GitHub secondary rate limit",
			status:     http.StatusForbidden,
			diagnostic: `{"message":"You have exceeded a secondary rate limit. Please wait."}`,
			rawURL:     "https://api.github.com/repos/oven-sh/bun/releases/tags/bun-v1.3.10",
		},
		{
			name:   "origin unavailable",
			status: http.StatusServiceUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rawURL := test.rawURL
			if rawURL == "" {
				rawURL = "https://registry.npmjs.org/npm/11.4.2"
			}
			diagnostic := test.diagnostic
			if diagnostic == "" {
				diagnostic = "diagnostic"
			}
			client := &http.Client{Transport: upstreamRoundTripper(
				func(request *http.Request) (*http.Response, error) {
					return &http.Response{
						Body:       io.NopCloser(strings.NewReader(diagnostic)),
						Header:     test.headers.Clone(),
						Request:    request,
						StatusCode: test.status,
					}, nil
				},
			)}
			_, err := fetchUpstream(
				context.Background(),
				client,
				t.TempDir(),
				rawURL,
				[]string{"registry.npmjs.org", "api.github.com"},
				1024,
			)
			if err == nil {
				t.Fatal("fetchUpstream returned nil error")
			}
			value, ok := err.(interface {
				PlatformAcquisitionFailureReason() workerapi.PlatformAcquisitionFailureReason
			})
			if ok != test.deterministic {
				t.Fatalf("deterministic = %v, want %v: %v", ok, test.deterministic, err)
			}
			if ok && value.PlatformAcquisitionFailureReason() != test.reason {
				t.Fatalf(
					"reason = %q, want %q",
					value.PlatformAcquisitionFailureReason(),
					test.reason,
				)
			}
		})
	}
}
