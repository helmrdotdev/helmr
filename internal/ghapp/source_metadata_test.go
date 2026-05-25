package ghapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	ghapi "github.com/google/go-github/v75/github"
	"github.com/helmrdotdev/helmr/internal/api"
)

func TestParsePullRequestRef(t *testing.T) {
	number, ok := parsePullRequestRef("refs/pull/42/head")
	if !ok || number != 42 {
		t.Fatalf("number = %d ok = %v", number, ok)
	}
	number, ok = parsePullRequestRef("pull/7/head")
	if !ok || number != 7 {
		t.Fatalf("number = %d ok = %v", number, ok)
	}
	if _, ok := parsePullRequestRef("main"); ok {
		t.Fatal("expected main to be rejected as pull request ref")
	}
}

func TestResolveGitHubRefLiteralSHA(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/commits/"+testSHA {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"sha": testSHA}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	resolved, err := resolveGitHubRef(context.Background(), testGitHubClient(t, server.URL), "owner", "repo", testSHA)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.kind != api.GitHubRefKindSHA || resolved.sha != testSHA || resolved.refName != testSHA {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestResolveGitHubRefRejectsUnknownSHA(t *testing.T) {
	unknownSHA := "ffffffffffffffffffffffffffffffffffffffff"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/commits/"+unknownSHA {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	_, err := resolveGitHubRef(context.Background(), testGitHubClient(t, server.URL), "owner", "repo", unknownSHA)
	if err == nil || !IsInvalidSource(err) {
		t.Fatalf("err = %v, want invalid source", err)
	}
	if !strings.Contains(err.Error(), unknownSHA) {
		t.Fatalf("err = %v, want sha in message", err)
	}
}

func testGitHubClient(t *testing.T, rawURL string) *ghapi.Client {
	t.Helper()
	baseURL, err := url.Parse(rawURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client := ghapi.NewClient(nil).WithAuthToken("token")
	client.BaseURL = baseURL
	return client
}
