package ghapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestResolverResolvesBranchWithInstallationToken(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/app/installations/123/access_tokens":
			if got := r.Header.Get("authorization"); !strings.HasPrefix(got, "Bearer ") || strings.Count(got, ".") != 2 {
				t.Fatalf("authorization = %q, want app jwt", got)
			}
			var body struct {
				RepositoryIDs []int64 `json:"repository_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 456 {
				t.Fatalf("repository_ids = %+v", body.RepositoryIDs)
			}
			if err := json.NewEncoder(w).Encode(map[string]any{
				"token":      "installation-token",
				"expires_at": "2026-05-08T12:30:00Z",
			}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr":
			if err := json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr/git/ref/heads/main":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"ref": "refs/heads/main",
				"object": map[string]string{
					"type": "commit",
					"sha":  testSHA,
				},
			}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	resolver := testResolver(t, server.URL)
	source, err := resolver.ResolveCommit(context.Background(), 123, 456, api.GitHubSource{Repository: "helmrdotdev/helmr", Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if source.Source.SHA != testSHA || source.Source.Ref != "main" || source.Source.Repository != "helmrdotdev/helmr" {
		t.Fatalf("source = %+v", source)
	}
	if source.Source.RefKind != api.GitHubRefKindBranch || source.Source.RefName != "main" || source.Source.FullRef != "refs/heads/main" {
		t.Fatalf("source metadata = %+v", source.Source)
	}
	if source.Source.DefaultBranch != "main" {
		t.Fatalf("default branch = %q", source.Source.DefaultBranch)
	}
	if strings.Join(paths, ",") != "/app/installations/123/access_tokens,/repos/helmrdotdev/helmr,/repos/helmrdotdev/helmr/git/ref/heads/main" {
		t.Fatalf("paths = %+v", paths)
	}
}

func TestResolverResolvesExplicitGitRef(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/app/installations/123/access_tokens":
			if err := json.NewEncoder(w).Encode(map[string]any{"token": "installation-token"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr":
			if err := json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr/git/ref/heads/main":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"ref": "refs/heads/main",
				"object": map[string]string{
					"type": "commit",
					"sha":  testSHA,
				},
			}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	resolver := testResolver(t, server.URL)
	source, err := resolver.ResolveCommit(context.Background(), 123, 456, api.GitHubSource{Repository: "helmrdotdev/helmr", Ref: "refs/heads/main"})
	if err != nil {
		t.Fatal(err)
	}
	if source.Source.SHA != testSHA || source.Source.Ref != "refs/heads/main" {
		t.Fatalf("source = %+v", source)
	}
	if strings.Join(paths, ",") != "/app/installations/123/access_tokens,/repos/helmrdotdev/helmr,/repos/helmrdotdev/helmr/git/ref/heads/main" {
		t.Fatalf("paths = %+v", paths)
	}
}

func TestResolverRejectsMissingInstallation(t *testing.T) {
	resolver := testResolver(t, "http://example.invalid")
	_, err := resolver.ResolveCommit(context.Background(), 0, 456, api.GitHubSource{Repository: "helmrdotdev/helmr", Ref: "main"})
	if err == nil || !IsInvalidSource(err) {
		t.Fatalf("err = %v, want invalid source", err)
	}
}

func TestResolverRejectsInvalidSource(t *testing.T) {
	resolver := testResolver(t, "http://example.invalid")
	_, err := resolver.ResolveCommit(context.Background(), 123, 456, api.GitHubSource{
		Repository: "not-a-repo",
		Ref:        "main",
	})
	if err == nil || !IsInvalidSource(err) {
		t.Fatalf("err = %v, want invalid source", err)
	}
}

func TestResolverRejectsInvalidGitHubSHA(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/123/access_tokens":
			if err := json.NewEncoder(w).Encode(map[string]any{"token": "installation-token"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr":
			if err := json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr/git/ref/heads/main":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"ref": "refs/heads/main",
				"object": map[string]string{
					"type": "commit",
					"sha":  "abc",
				},
			}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	resolver := testResolver(t, server.URL)
	_, err := resolver.ResolveCommit(context.Background(), 123, 456, api.GitHubSource{Repository: "helmrdotdev/helmr", Ref: "main"})
	if err == nil || !strings.Contains(err.Error(), "github resolved commit sha") {
		t.Fatalf("err = %v, want invalid resolved commit sha", err)
	}
}

func TestCreateRepositoryTokenReturnsExpiry(t *testing.T) {
	expiresAt := "2026-05-08T12:30:00Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/123/access_tokens" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"token":      "installation-token",
			"expires_at": expiresAt,
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	resolver := testResolver(t, server.URL)
	token, err := resolver.CreateRepositoryToken(context.Background(), 123, 456)
	if err != nil {
		t.Fatal(err)
	}
	wantExpiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "installation-token" || !token.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("token = %+v", token)
	}
}

func TestResolverVerifiesUserInstallation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/installations":
			if got := r.Header.Get("authorization"); got != "Bearer github-user-token" {
				t.Fatalf("authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"installations": []map[string]any{
					{
						"id":                   123,
						"app_id":               12345,
						"html_url":             "https://github.com/settings/installations/123",
						"repository_selection": "selected",
						"account": map[string]any{
							"login": "helmrdotdev",
							"type":  "Organization",
						},
					},
				},
			})
		case "/user/installations/123/repositories":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"repositories": []map[string]any{
					{
						"id":             456,
						"name":           "helmr",
						"full_name":      "helmrdotdev/helmr",
						"default_branch": "main",
						"owner": map[string]any{
							"login": "helmrdotdev",
						},
					},
				},
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()
	resolver := testResolver(t, server.URL)

	verified, err := resolver.VerifyUserInstallation(context.Background(), "github-user-token", 123)
	if err != nil {
		t.Fatal(err)
	}
	if verified.InstallationID != 123 || verified.AccountLogin != "helmrdotdev" || verified.AccountType != "Organization" || verified.RepositorySelection != "selected" || verified.HTMLURL == "" {
		t.Fatalf("verified = %+v", verified)
	}
	if len(verified.Repositories) != 1 || verified.Repositories[0].ID != 456 {
		t.Fatalf("repositories = %+v", verified.Repositories)
	}
}

func TestResolverResolvesPullRequestRef(t *testing.T) {
	const prHeadSHA = "abcdef0123456789abcdef0123456789abcdef01"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/app/installations/123/access_tokens":
			if err := json.NewEncoder(w).Encode(map[string]any{"token": "installation-token"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr":
			if err := json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr/git/ref/pull/42/head":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"ref": "refs/pull/42/head",
				"object": map[string]string{
					"type": "commit",
					"sha":  prHeadSHA,
				},
			}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr/pulls/42":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"number": 42,
				"base": map[string]string{
					"ref": "main",
					"sha": testSHA,
				},
				"head": map[string]string{
					"ref": "feature-branch",
					"sha": prHeadSHA,
				},
			}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	resolver := testResolver(t, server.URL)
	source, err := resolver.ResolveCommit(context.Background(), 123, 456, api.GitHubSource{
		Repository: "helmrdotdev/helmr",
		Ref:        "pull/42/head",
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.Source.SHA != prHeadSHA {
		t.Fatalf("sha = %q, want %q", source.Source.SHA, prHeadSHA)
	}
	if source.Source.RefKind != api.GitHubRefKindPullRequest {
		t.Fatalf("ref kind = %q, want pull_request", source.Source.RefKind)
	}
	if source.Source.RefName != "42" || source.Source.FullRef != "refs/pull/42/head" {
		t.Fatalf("ref metadata = %+v", source.Source)
	}
	if source.Source.PullRequest == nil {
		t.Fatal("expected pull request metadata")
	}
	if source.Source.PullRequest.Number != 42 ||
		source.Source.PullRequest.BaseRef != "main" ||
		source.Source.PullRequest.BaseSHA != testSHA ||
		source.Source.PullRequest.HeadRef != "feature-branch" ||
		source.Source.PullRequest.HeadSHA != prHeadSHA {
		t.Fatalf("pull request metadata = %+v", source.Source.PullRequest)
	}
	wantPaths := "/app/installations/123/access_tokens,/repos/helmrdotdev/helmr,/repos/helmrdotdev/helmr/git/ref/pull/42/head,/repos/helmrdotdev/helmr/pulls/42"
	if strings.Join(paths, ",") != wantPaths {
		t.Fatalf("paths = %+v, want %s", paths, wantPaths)
	}
}

func TestResolverResolvesTagRef(t *testing.T) {
	var paths []string
	const tagSHA = "fedcba9876543210fedcba9876543210fedcba98"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/app/installations/123/access_tokens":
			if err := json.NewEncoder(w).Encode(map[string]any{"token": "installation-token"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr":
			if err := json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr/git/ref/heads/v1.0.0":
			http.Error(w, "not found", http.StatusNotFound)
		case "/repos/helmrdotdev/helmr/git/ref/tags/v1.0.0":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"ref": "refs/tags/v1.0.0",
				"object": map[string]string{
					"type": "commit",
					"sha":  tagSHA,
				},
			}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	resolver := testResolver(t, server.URL)
	source, err := resolver.ResolveCommit(context.Background(), 123, 456, api.GitHubSource{Repository: "helmrdotdev/helmr", Ref: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if source.Source.SHA != tagSHA {
		t.Fatalf("sha = %q, want %q", source.Source.SHA, tagSHA)
	}
	if source.Source.RefKind != api.GitHubRefKindTag || source.Source.RefName != "v1.0.0" || source.Source.FullRef != "refs/tags/v1.0.0" {
		t.Fatalf("source metadata = %+v", source.Source)
	}
	wantPaths := "/app/installations/123/access_tokens,/repos/helmrdotdev/helmr,/repos/helmrdotdev/helmr/git/ref/heads/v1.0.0,/repos/helmrdotdev/helmr/git/ref/tags/v1.0.0"
	if strings.Join(paths, ",") != wantPaths {
		t.Fatalf("paths = %+v, want %s", paths, wantPaths)
	}
}

func TestResolverResolvesExplicitTagRef(t *testing.T) {
	var paths []string
	const tagSHA = "fedcba9876543210fedcba9876543210fedcba98"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/app/installations/123/access_tokens":
			if err := json.NewEncoder(w).Encode(map[string]any{"token": "installation-token"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr":
			if err := json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"}); err != nil {
				t.Fatal(err)
			}
		case "/repos/helmrdotdev/helmr/git/ref/tags/v1.0.0":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"ref": "refs/tags/v1.0.0",
				"object": map[string]string{
					"type": "commit",
					"sha":  tagSHA,
				},
			}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	resolver := testResolver(t, server.URL)
	source, err := resolver.ResolveCommit(context.Background(), 123, 456, api.GitHubSource{Repository: "helmrdotdev/helmr", Ref: "refs/tags/v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if source.Source.SHA != tagSHA {
		t.Fatalf("sha = %q, want %q", source.Source.SHA, tagSHA)
	}
	if source.Source.RefKind != api.GitHubRefKindTag || source.Source.RefName != "v1.0.0" || source.Source.FullRef != "refs/tags/v1.0.0" {
		t.Fatalf("source metadata = %+v", source.Source)
	}
	wantPaths := "/app/installations/123/access_tokens,/repos/helmrdotdev/helmr,/repos/helmrdotdev/helmr/git/ref/tags/v1.0.0"
	if strings.Join(paths, ",") != wantPaths {
		t.Fatalf("paths = %+v, want %s", paths, wantPaths)
	}
}

func TestResolverRejectsSpoofedUserInstallation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"installations": []map[string]any{
				{"id": 456, "app_id": 12345},
			},
		})
	}))
	defer server.Close()
	resolver := testResolver(t, server.URL)

	_, err := resolver.VerifyUserInstallation(context.Background(), "github-user-token", 123)
	if !IsInvalidSource(err) {
		t.Fatalf("error = %v", err)
	}
}

func testResolver(t *testing.T, rawURL string) *Resolver {
	t.Helper()
	baseURL, err := url.Parse(rawURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver("12345", "helmr-test", testPrivateKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	resolver.baseURL = baseURL
	resolver.now = func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) }
	return resolver
}

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
