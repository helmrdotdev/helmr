package controlplane

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSlackIdentityCallbackBridgesQueryAndFormWithoutAuthentication(t *testing.T) {
	cfg := completeServerConfig(t)
	cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			for _, test := range []struct {
				name, values string
				status       int
			}{
				{"code", "code=private-code&state=flow", 303},
				{"error", "error=access_denied&state=flow", 303},
				{"duplicate_code", "code=a&code=b", 400},
				{"duplicate_state", "state=a&state=b", 400},
				{"duplicate_error", "error=a&error=b", 400},
				{"malformed", "code=%GG", 400},
				{"long_code", "code=" + strings.Repeat("a", 4097), 400},
				{"large_input", "unused=" + strings.Repeat("a", 8193), 400},
			} {
				t.Run(test.name, func(t *testing.T) {
					path := "https://console.example.test/api/slack/user-links/callback"
					body := ""
					if method == "GET" {
						path += "?" + test.values
					} else {
						body = test.values
					}
					req := httptest.NewRequest(method, path, strings.NewReader(body))
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					result := httptest.NewRecorder()
					handler.ServeHTTP(result, req)
					if result.Code != test.status {
						t.Fatalf("status %d: %s", result.Code, result.Body.String())
					}
					if result.Header().Get("Cache-Control") != "no-store" || result.Header().Get("Referrer-Policy") != "no-referrer" {
						t.Fatal("missing callback privacy headers")
					}
					if result.Code == 303 {
						target, err := url.Parse(result.Header().Get("Location"))
						if err != nil || target.Scheme != "https" || target.Host != "console.example.test" || target.Path != "/auth/slack/link" || target.RawQuery != "" {
							t.Fatalf("unsafe bridge: %v", err)
						}
						fields, err := url.ParseQuery(target.Fragment)
						if err != nil || fields.Encode() != test.values {
							t.Fatal("callback fields changed")
						}
					}
				})
			}
		})
	}
}
