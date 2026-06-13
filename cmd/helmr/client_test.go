package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestAPIURLFlagOverridesEnvironmentURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/runs/run-1/logs" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer env-key" {
			t.Fatalf("auth = %s", got)
		}
		_ = json.NewEncoder(w).Encode(api.LogSnapshotResponse{
			StdoutBase64: base64.StdEncoding.EncodeToString([]byte("from flag\n")),
			StderrBase64: "",
		})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, "https://ignored.example.test")
	t.Setenv(helmrAPIKeyEnv, "env-key")

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--api-url", server.URL, "logs", "run-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "from flag\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestControlClientRejectsPlainHTTPNonLoopback(t *testing.T) {
	t.Setenv(helmrAPIURLEnv, "http://helmr.example")
	t.Setenv(helmrAPIKeyEnv, "test-key")

	_, err := controlClient(nil)
	if err == nil || !strings.Contains(err.Error(), "plaintext non-loopback") {
		t.Fatalf("err = %v", err)
	}
}

func TestControlClientRejectsURLQueryAndFragment(t *testing.T) {
	t.Setenv(helmrAPIKeyEnv, "test-key")
	for _, raw := range []string{"https://helmr.example?x=1", "https://helmr.example/#fragment"} {
		t.Setenv(helmrAPIURLEnv, raw)
		_, err := controlClient(nil)
		if err == nil || !strings.Contains(err.Error(), "must not include query or fragment") {
			t.Fatalf("controlClient(%q) err = %v", raw, err)
		}
	}
}
